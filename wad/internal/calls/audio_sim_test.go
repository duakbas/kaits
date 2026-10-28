package calls

// Offline simulation of the daemon's WhatsApp -> phone audio path, using the
// real jitter queue (latency target and trim included) and pump cadence — no
// WhatsApp, no phone, no network. The phone-leg codec (Opus/G.722/PCMU) is
// not in the loop: what's written is the PCM the pump hands the leg.
//
//	WAD_AUDIO_SIM=/tmp/sim go test -run TestAudioSim -count=1 -v ./internal/calls
//
// Skipped unless WAD_AUDIO_SIM is set. Writes, into that directory:
//
//	ref_16k.wav          the test signal, as meowcaller would hand it over
//	down_<scenario>.wav  16 kHz: exactly what the pump hands the phone leg
//
// The downlink is driven in virtual time: meowcaller frames (60 ms) arrive with
// a chosen jitter, loss and clock drift, and the pump drains the queue on its
// own 20 ms clock — the same race that happens on the VPS, but repeatable.
// Analyse the WAVs with tools/audio_check.py (its `server` mode runs this).

import (
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

const simRate = 16000
const simFrame = 960 // meowcaller frame: 60 ms at 16 kHz

type simScenario struct {
	name     string
	jitterMs float64 // std dev of late arrival, half-normal
	lossPct  float64
	driftPPM float64 // >0: WhatsApp side delivers slower than our ticker
	legUpMs  float64 // audio arriving before the phone leg is up (ICE+DTLS)
}

var simScenarios = []simScenario{
	{"clean", 0, 0, 0, 0},
	{"jitter5", 5, 0, 0, 0},
	{"jitter15", 15, 0, 0, 0},
	{"jitter30", 30, 0, 0, 0},
	{"loss2", 5, 2, 0, 0},
	{"drift", 3, 0, 300, 0},
	{"legup800", 5, 0, 0, 800},
}

type simStats struct {
	ticks, starved, partial, lost int
	droppedSamples, trimmed       int
	maxDepth                      int
}

// simSignal: 2 s of 1 kHz tone (every glitch is audible and measurable on a
// pure tone), 4 s log sweep 100 Hz -> 7.5 kHz, 6 s of voiced "speech":
// harmonics of a gliding pitch under a syllable envelope.
func simSignal() []float32 {
	var out []float32
	for i := 0; i < 2*simRate; i++ {
		out = append(out, float32(0.4*math.Sin(2*math.Pi*1000*float64(i)/simRate)))
	}
	f0, f1, T := 100.0, 7500.0, 4.0
	k := math.Log(f1 / f0)
	for i := 0; i < int(T*simRate); i++ {
		t := float64(i) / simRate
		ph := 2 * math.Pi * f0 * T / k * (math.Exp(t/T*k) - 1)
		out = append(out, float32(0.4*math.Sin(ph)))
	}
	var ph float64
	for i := 0; i < 6*simRate; i++ {
		t := float64(i) / simRate
		pitch := 140 + 25*math.Sin(2*math.Pi*0.7*t)
		ph += 2 * math.Pi * pitch / simRate
		env := 0.15 + 0.85*math.Pow(math.Sin(math.Pi*4*t), 2)
		var s float64
		for h := 1; float64(h)*pitch < 3800; h++ {
			s += math.Sin(float64(h)*ph) / float64(h)
		}
		out = append(out, float32(0.18*env*s))
	}
	return out[:len(out)/simFrame*simFrame]
}

// simDown runs the WhatsApp -> phone direction through the real queue and pump
// logic (answerRealCall's push, pump's take) in virtual microseconds.
func simDown(sig []float32, sc simScenario, rng *rand.Rand) ([]int16, simStats) {
	q := newJitter(LegRate) // same as answerRealCall
	var st simStats

	n := len(sig) / simFrame
	arr := make([]int64, n)
	period := 60000 * (1 + sc.driftPPM/1e6)
	for i := range arr {
		t := float64(i)*period + math.Abs(rng.NormFloat64())*sc.jitterMs*1000
		arr[i] = int64(t)
		if i > 0 && arr[i] < arr[i-1] {
			arr[i] = arr[i-1] // meowcaller hands frames over in order
		}
	}
	lost := make([]bool, n)
	for i := range lost {
		lost[i] = rng.Float64()*100 < sc.lossPct
	}

	var out []int16
	ai := 0
	started := false
	// The ticker's phase relative to the relay is arbitrary; 7 ms is as good
	// as any and keeps arrivals off tick boundaries.
	for t := int64(7000); ai < n || q.depth() > 0; t += 20000 {
		for ai < n && arr[ai] <= t {
			if lost[ai] {
				st.lost++
			} else {
				p := f32to16(sig[ai*simFrame : (ai+1)*simFrame])
				if over := q.depth() + len(p) - LegRate; over > 0 {
					st.droppedSamples += over
				}
				q.push(p)
				started = true
			}
			ai++
		}
		if !started || float64(t) < sc.legUpMs*1000 {
			continue
		}
		before := q.depth()
		if before > st.maxDepth {
			st.maxDepth = before
		}
		f, real := q.take(LegFrame)
		st.ticks++
		if !real {
			st.starved++
		} else if before < LegFrame {
			st.partial++
		}
		out = append(out, f...)
	}
	st.trimmed = q.takeTrimmed()
	return out, st
}

func TestAudioSim(t *testing.T) {
	dir := os.Getenv("WAD_AUDIO_SIM")
	if dir == "" {
		t.Skip("set WAD_AUDIO_SIM=<dir> to run the audio simulation")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sig := simSignal()
	ref := make([]int16, len(sig))
	for i, s := range sig {
		ref[i] = clamp16(float64(s) * 32767)
	}
	writeWAV(t, filepath.Join(dir, "ref_16k.wav"), simRate, ref)

	for _, sc := range simScenarios {
		out, st := simDown(sig, sc, rand.New(rand.NewSource(1)))
		writeWAV(t, filepath.Join(dir, "down_"+sc.name+".wav"), LegRate, out)
		t.Logf("%-9s ticks %4d  starved %3d  partial %3d  lost %2d  trimmed %4.0f ms  max queue %4.0f ms",
			sc.name, st.ticks, st.starved, st.partial, st.lost,
			float64(st.trimmed+st.droppedSamples)*1000/LegRate, float64(st.maxDepth)*1000/LegRate)
	}
}

func writeWAV(t *testing.T, path string, rate int, pcm []int16) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := uint32(len(pcm) * 2)
	hdr := []any{
		[4]byte{'R', 'I', 'F', 'F'}, 36 + data, [4]byte{'W', 'A', 'V', 'E'},
		[4]byte{'f', 'm', 't', ' '}, uint32(16), uint16(1), uint16(1),
		uint32(rate), uint32(rate * 2), uint16(2), uint16(16),
		[4]byte{'d', 'a', 't', 'a'}, data,
	}
	for _, v := range hdr {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := binary.Write(f, binary.LittleEndian, pcm); err != nil {
		t.Fatal(err)
	}
}
