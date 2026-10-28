#!/usr/bin/env python3
"""
call_sim.py — mock WhatsApp call through wad, end to end, with a lossy network.

    caller --(WhatsApp relay: loss, jitter, reorder)--> meowcaller --> wad
         --(Opus over mobile data: loss, jitter)--> phone jitter buffer + Opus decoder

Two wad designs on the same network trace:

  current   meowcaller's playout buffer -> wad's jitter queue -> 20 ms pump,
            zero-fill on starvation, continuous RTP timestamps (what runs today)
  passthru  each WhatsApp frame forwarded on arrival as 3x20 ms Opus packets
            stamped with WhatsApp's own RTP timestamp; the phone's jitter
            buffer sees real lateness and loss and conceals with Opus PLC/FEC

and three phone-leg redundancy options for passthru: none, Opus in-band FEC
(wad's current encoder setting), and packet duplication.

meowcaller's playout buffer (audio_playout.go @27a3c6b) and wad's jitter queue
(join.go) are line-by-line ports; `--verify` checks them sample-exact against
the Go originals if Go is available. The phone side is a stand-in for Gecko's
NetEQ (fixed target delay, Opus PLC/FEC, late packets dropped) — the real
NetEQ also time-stretches, so it does somewhat better than this on jitter.

    python3 call_sim.py                       # run all scenarios
    python3 call_sim.py --wa-loss 1.2 --wa-jitter 9 --phone-loss 0.8 --phone-jitter 14
                                              # plug in measured numbers (%, ms)
    python3 call_sim.py --wav out/            # also write WAVs to listen to

Deps: numpy scipy opuslib pesq   (opuslib needs the libopus shared library)
"""
import argparse, os, sys, warnings
import numpy as np
from scipy import signal
from scipy.io import wavfile

warnings.filterwarnings("ignore", category=SyntaxWarning)
import opuslib  # noqa: E402

SR = 16000
WA_FRAME = 960     # meowcaller: 60 ms
LEG_FRAME = 320    # phone leg: 20 ms
OPUS_BITRATE = 32000


# ---------------------------------------------------------------- test signals
def sine(sec):
    t = np.arange(int(sec * SR)) / SR
    return 0.4 * np.sin(2 * np.pi * 1000 * t)


def speech(sec, seed=3):
    """Formant-synthesised speech: glottal pulses through vowel resonances,
    pitch contour, noisy consonants, syllable rhythm and pauses."""
    rng = np.random.default_rng(seed)
    vowels = [(730, 1090, 2440), (270, 2290, 3010), (300, 870, 2240),
              (530, 1840, 2480), (570, 840, 2410), (660, 1720, 2410)]
    out = []
    while sum(len(x) for x in out) < sec * SR:
        if rng.random() < 0.12:                       # pause between phrases
            out.append(np.zeros(int(rng.uniform(0.15, 0.4) * SR)))
            continue
        if rng.random() < 0.6:                        # consonant: shaped noise
            n = int(rng.uniform(0.03, 0.09) * SR)
            b, a = signal.butter(2, [rng.uniform(1500, 3000), 7000], "band", fs=SR)
            c = signal.lfilter(b, a, rng.standard_normal(n)) * np.hanning(n) * 0.08
            out.append(c)
        n = int(rng.uniform(0.12, 0.28) * SR)         # vowel
        f0 = rng.uniform(100, 180) * np.linspace(1.05, 0.9, n)
        ph = np.cumsum(2 * np.pi * f0 / SR)
        src = (ph / (2 * np.pi)) % 1.0
        src = (src < 0.4) * np.sin(np.pi * src / 0.4) ** 2   # glottal pulse
        src -= src.mean()
        v = src
        for f, bw in zip(vowels[rng.integers(len(vowels))], (80, 100, 120)):
            r = np.exp(-np.pi * bw / SR)
            v = signal.lfilter([1 - r], [1, -2 * r * np.cos(2 * np.pi * f / SR), r * r], v) * 3
        env = np.minimum(1, np.minimum(np.arange(n), n - np.arange(n)) / (0.02 * SR))
        out.append(v / (np.abs(v).max() + 1e-9) * 0.35 * env)
    x = np.concatenate(out)[: int(sec * SR)]
    return x / np.abs(x).max() * 0.5


# ------------------------------------------------------------- network models
def gilbert(n, loss_pct, burst, rng):
    """Bursty loss: Gilbert-Elliott with mean burst length `burst` packets."""
    if loss_pct <= 0:
        return np.zeros(n, bool)
    p_bg = 1 / burst
    p_gb = p_bg * loss_pct / 100 / (1 - loss_pct / 100)
    lost, bad = np.zeros(n, bool), False
    for i in range(n):
        bad = (rng.random() < p_gb) if not bad else (rng.random() >= p_bg)
        lost[i] = bad
    return lost


def delays(n, base_ms, jitter_ms, rng):
    """Base latency + heavy-tailed jitter (gamma, mean = jitter_ms)."""
    if jitter_ms <= 0:
        return np.full(n, base_ms)
    k = 1.5
    return base_ms + rng.gamma(k, jitter_ms / k, n)


# --------------------------------------------- meowcaller playout (exact port)
class MeowPlayout:
    PREFILL, MAXGAP = 2 * WA_FRAME, 2 * SR

    def __init__(self):
        self.pending, self.prefill, self.prefill_n, self.started = None, [], 0, False

    def push(self, ts, frame, sink):
        if self.pending is None:
            self.pending = (ts, frame)
            return
        delta = (ts - self.pending[0]) & 0xFFFFFFFF
        if delta == 0 or delta > self.MAXGAP:
            self.pending, self.prefill, self.prefill_n, self.started = (ts, frame), [], 0, False
            return
        ready = self._align(self.pending[1], delta)
        self.pending = (ts, frame)
        if self.started:
            sink(ready)
            return
        self.prefill.append(ready)
        self.prefill_n += len(ready)
        while self.prefill_n > self.MAXGAP and len(self.prefill) > 1:
            self.prefill_n -= len(self.prefill.pop(0))
        if self.prefill_n < self.PREFILL:
            return
        for b in self.prefill:
            sink(b)
        self.started, self.prefill, self.prefill_n = True, [], 0

    @staticmethod
    def _align(frame, samples):
        samples = min(samples, max(len(frame), WA_FRAME))
        if samples == len(frame):
            return frame
        out = np.zeros(samples)
        out[: min(samples, len(frame))] = frame[:samples]
        return out


# ------------------------------------------------- wad jitter queue (exact port)
class WadJitter:
    def __init__(self, mx=SR):
        self.buf = np.zeros(0, np.int16)
        self.max, self.target, self.high = mx, mx * 80 // 1000, mx * 200 // 1000
        self.over = self.trimmed = 0

    def push(self, s):
        self.buf = np.concatenate([self.buf, s])
        if len(self.buf) > self.max:
            self.buf = self.buf[-self.max:]

    def take(self, n):
        if len(self.buf) > self.high:
            self.over += 1
            if self.over >= 25:
                cut = len(self.buf) - self.target
                self.buf, self.trimmed, self.over = self.buf[cut:], self.trimmed + cut, 0
        else:
            self.over = 0
        out = np.zeros(n, np.int16)
        if len(self.buf) == 0:
            return out, False
        c = min(n, len(self.buf))
        out[:c], self.buf = self.buf[:c], self.buf[c:]
        return out, True


def f32to16(x):
    return np.clip(np.asarray(x, np.float64) * 32767, -32768, 32767).astype(np.int16)


# ----------------------------------------------------------------- opus helpers
def encoder(fec):
    e = opuslib.Encoder(SR, 1, "voip")
    e.bitrate, e.complexity = OPUS_BITRATE, 10
    # opuslib's property setters for these two are broken; call the ctl directly
    ctl, api = opuslib.api.encoder.encoder_ctl, opuslib.api.ctl
    ctl(e.encoder_state, api.set_inband_fec, 1 if fec else 0)
    ctl(e.encoder_state, api.set_packet_loss_perc, 5 if fec else 0)  # wad's values
    return e


def phone_receive(packets, n_slots, target_ms, fec):
    """Stand-in for Gecko's jitter buffer: slot n plays at first_arrival +
    target + 20n ms. A packet arriving by then is decoded; otherwise the next
    packet's FEC is used if present, else Opus PLC. Late packets are dropped.
    packets: {slot: (arrival_ms, payload)} (earliest copy wins)."""
    d = opuslib.Decoder(SR, 1)
    t0 = min(a for a, _ in packets.values()) + target_ms
    first = min(packets)
    out, plc = [], 0
    for n in range(first, n_slots):
        deadline = t0 + (n - first) * 20
        p, nx = packets.get(n), packets.get(n + 1)
        if p and p[0] <= deadline:
            pcm = d.decode(p[1], LEG_FRAME)
        elif fec and nx and nx[0] <= deadline:
            pcm = d.decode(nx[1], LEG_FRAME, decode_fec=True)
            plc += 1
        else:
            pcm = d.decode(b"", LEG_FRAME)
            plc += 1
        out.append(np.frombuffer(pcm, np.int16))
    return np.concatenate(out), plc


def leg_send(frames_by_slot, send_ms, net, rng, fec, dup_ms=0):
    """Encode 20 ms frames (in slot order) and push them over the phone leg."""
    e = encoder(fec)
    slots = sorted(frames_by_slot)
    n = len(slots)
    tmax = max(send_ms.values()) + 1000
    # loss state follows wall-clock time (20 ms cells) so a duplicate sent
    # later sees a different, but still bursty, channel
    cells = gilbert(int(tmax // 20) + 2, net["phone_loss"], 2.0, rng)
    dl = delays(2 * (max(slots) + 2), net["phone_base"], net["phone_jitter"], rng)
    packets = {}
    for s in slots:
        payload = e.encode(frames_by_slot[s].astype(np.int16).tobytes(), LEG_FRAME)
        for copy, off in enumerate((0, dup_ms) if dup_ms else (0,)):
            t = send_ms[s] + off
            if not cells[int(t // 20)]:
                arr = t + dl[2 * s + copy]
                if s not in packets or arr < packets[s][0]:
                    packets[s] = (arr, payload)
    return packets


# --------------------------------------------------------------- the two paths
def wa_trace(n, net, rng):
    lost = gilbert(n, net["wa_loss"], 1.6, rng)
    arr = np.arange(n) * 60.0 + delays(n, net["wa_base"], net["wa_jitter"], rng)
    reo = rng.random(n) < net["wa_reorder"] / 100
    arr[reo] += 75  # pushed behind its successor
    return [(arr[i], i) for i in range(n) if not lost[i]], int(lost.sum()), int(reo.sum())


def path_current(sig16, trace, net, rng, fec=True):
    q, mp = WadJitter(), MeowPlayout()
    events = sorted(trace)
    t_tick, ev, tick = events[0][0] + 7, 0, 0
    frames, send = {}, {}
    sink = lambda pcm: q.push(f32to16(pcm))
    starved = 0
    while ev < len(events) or len(q.buf):
        while ev < len(events) and events[ev][0] <= t_tick:
            i = events[ev][1]
            mp.push(i * WA_FRAME, sig16[i * WA_FRAME:(i + 1) * WA_FRAME], sink)
            ev += 1
        f, real = q.take(LEG_FRAME)
        starved += not real
        frames[tick], send[tick] = f, t_tick
        tick, t_tick = tick + 1, t_tick + 20
        if tick > len(sig16) // LEG_FRAME + 200:
            break
    packets = leg_send(frames, send, net, rng, fec)
    target = np.percentile([a - send[s] for s, (a, _) in packets.items()], 95)
    out, plc = phone_receive(packets, tick, target, fec)
    return out, {"starved": starved, "plc": plc, "trimmed": q.trimmed, "delay": target}


def path_passthru(sig16, trace, net, rng, fec=True, dup_ms=0):
    frames, send = {}, {}
    last, dropped = -1, 0
    for t, i in sorted(trace):
        if i <= last:            # arrived after a newer frame was already sent
            dropped += 1
            continue
        last = i
        for k in range(3):
            s = i * 3 + k
            frames[s] = f32to16(sig16[s * LEG_FRAME:(s + 1) * LEG_FRAME])
            send[s] = t
    packets = leg_send(frames, send, net, rng, fec, dup_ms)
    nominal = {s: s * 20 for s in packets}
    off = min(a - nominal[s] for s, (a, _) in packets.items())
    target = np.percentile([a - nominal[s] - off for s, (a, _) in packets.items()], 95)
    out, plc = phone_receive(packets, len(sig16) // LEG_FRAME, target, fec)
    return out, {"starved": 0, "plc": plc, "trimmed": 0, "dropped": dropped, "delay": target}


# --------------------------------------------------------------------- scoring
def pops(x):
    n = SR // 1000
    k = len(x) // n
    lv = 20 * np.log10(np.maximum(np.sqrt(((x[: k * n].astype(float) / 32768).reshape(k, n) ** 2).mean(1)), 1e-9))
    loud, quiet = lv > -45, lv < -70
    return int(((loud[:-1] & quiet[1:]) | (quiet[:-1] & loud[1:])).sum())


def glitches(x):
    """On the steady sine: every dip of the 2.5 ms level below half the
    nominal amplitude is one audible glitch (dropout, cut, or concealment
    fading out). Needs no time alignment, so it works on any path."""
    n = int(SR * 0.0025)
    k = len(x) // n
    lv = np.sqrt(((x[: k * n].astype(float)).reshape(k, n) ** 2).mean(1))
    on = np.where(lv > 0.1 * lv.max())[0]
    lv = lv[on[0]: on[-1] + 1]                       # ignore lead-in/tail
    low = lv < 0.5 * np.median(lv)
    events = int((low[1:] & ~low[:-1]).sum() + low[0])
    return events, int(low.sum() * 2.5)


def mos(ref, deg):
    try:
        from pesq import pesq
    except ImportError:
        return float("nan")
    a, b = deg.astype(float) / 32768, ref
    c = signal.fftconvolve(a, b[::-1], "full")
    lag = np.argmax(np.abs(c)) - (len(b) - 1)
    a = a[lag:] if lag >= 0 else np.r_[np.zeros(-lag), a]
    m = min(len(a), len(b))
    try:
        return pesq(SR, b[:m], a[:m], "wb")
    except Exception:
        return float("nan")


SCENARIOS = {
    #            WhatsApp relay leg               phone leg (mobile data)
    "good":    dict(wa_loss=0.5, wa_jitter=5,  wa_reorder=0.3, phone_loss=0.3, phone_jitter=8),
    "typical": dict(wa_loss=2.0, wa_jitter=15, wa_reorder=1.0, phone_loss=1.0, phone_jitter=20),
    "bad":     dict(wa_loss=5.0, wa_jitter=35, wa_reorder=2.0, phone_loss=3.0, phone_jitter=40),
}


def run(name, net, secs, seeds, wavdir):
    net = dict(wa_base=25, phone_base=35, **net)
    print(f"\n== {name}: relay loss {net['wa_loss']}% jitter {net['wa_jitter']} ms reorder "
          f"{net['wa_reorder']}% | phone loss {net['phone_loss']}% jitter {net['phone_jitter']} ms"
          f"   ({len(seeds)} calls x {secs:.0f} s, averaged)")
    clean = dict(net, wa_loss=0, wa_jitter=0, wa_reorder=0, phone_loss=0, phone_jitter=0)
    variants = [
        ("no loss (ceiling)",         lambda s, tr, r: path_passthru(s, tr, clean, r, fec=False)),
        ("current (today)",           lambda s, tr, r: path_current(s, tr, net, r, fec=True)),
        ("passthru, no redundancy",   lambda s, tr, r: path_passthru(s, tr, net, r, fec=False)),
        ("passthru + Opus FEC",       lambda s, tr, r: path_passthru(s, tr, net, r, fec=True)),
        ("passthru + duplicate 40ms", lambda s, tr, r: path_passthru(s, tr, net, r, fec=False, dup_ms=40)),
    ]
    sigs = {"sine": sine(secs), "voice": speech(secs)}
    n = len(sigs["sine"]) // WA_FRAME
    full = [(i * 60.0 + net["wa_base"], i) for i in range(n)]
    acc = {label: [] for label, _ in variants}
    for seed in seeds:
        trace, _, _ = wa_trace(n, net, np.random.default_rng(seed))
        for label, fn in variants:
            res = {}
            for kind, s in sigs.items():
                out, st = fn(s, full if label.startswith("no loss") else trace,
                             np.random.default_rng(seed + 1000))
                res[kind] = (out, st)
                if wavdir and seed == seeds[0]:
                    os.makedirs(wavdir, exist_ok=True)
                    tag = label.split()[0] + ("_" + label.split()[-1] if "+" in label or "," in label else "")
                    wavfile.write(os.path.join(wavdir, f"{name}_{tag}_{kind}.wav".replace(",", "")), SR, out)
            g, gms = glitches(res["sine"][0])
            st = res["voice"][1]
            acc[label].append((g * 60 / secs, gms * 60 / secs, mos(sigs["voice"], res["voice"][0]),
                               st["starved"] * 20 * 60 / secs, st["delay"]))
    print(f"  {'design':<26}{'glitches/min':>13}{'glitch s/min':>13}{'MOS':>6}{'zero-fill s/min':>16}{'buffer':>8}")
    for label, rows in acc.items():
        g, gms, m, zf, d = np.nanmean(np.array(rows, float), axis=0)
        print(f"  {label:<26}{g:>13.1f}{gms/1000:>13.2f}{m:>6.2f}{zf/1000:>16.2f}{d:>5.0f} ms")


def verify():
    """Check the Python ports against the Go originals on one trace."""
    import json, shutil, subprocess, tempfile
    if not shutil.which("go"):
        sys.exit("needs go")
    here = os.path.dirname(os.path.abspath(__file__))
    meow = os.environ.get("MEOW_PLAYOUT")  # path to meowcaller/audio_playout.go
    join = os.path.join(here, "..", "internal", "calls", "join.go")
    if not meow or not os.path.exists(meow):
        sys.exit("set MEOW_PLAYOUT=/path/to/meowcaller/audio_playout.go")
    rng = np.random.default_rng(7)
    sig = speech(6)
    trace, _, _ = wa_trace(len(sig) // WA_FRAME, SCENARIOS["bad"] | dict(wa_base=25), rng)
    d = tempfile.mkdtemp()
    open(f"{d}/go.mod", "w").write("module v\ngo 1.21\n")
    shutil.copy(meow, f"{d}/playout.go")
    src = open(join).read()
    jit = src[src.index("type jitter struct"): src.index("// answerRealCall")]
    open(f"{d}/jit.go", "w").write("package meowcaller\nimport \"sync\"\n" + jit)
    open(f"{d}/shim.go", "w").write("package meowcaller\nconst SampleRate=16000\nconst FrameSamples=960\n"
                                    "type AudioSink interface{ WriteFrame([]float32) error }\n")
    json.dump({"sig": sig.astype(np.float32).tolist(), "ev": [[t, i] for t, i in sorted(trace)]}, open(f"{d}/in.json", "w"))
    open(f"{d}/v_test.go", "w").write('''package meowcaller
import ("encoding/json";"os";"testing")
type sk struct{ q *jitter }
func (s sk) WriteFrame(f []float32) error { o := make([]int16, len(f)); for i, v := range f { x := float64(v)*32767; if x > 32767 { x = 32767 }; if x < -32768 { x = -32768 }; o[i] = int16(x) }; s.q.push(o); return nil }
func TestV(t *testing.T) {
	var in struct{ Sig []float32; Ev [][2]float64 }
	b, _ := os.ReadFile("in.json"); json.Unmarshal(b, &in)
	q := newJitter(16000); p := newAudioPlayoutBuffer(); s := sk{q}
	tick := in.Ev[0][0] + 7; ev := 0; var out []int16
	for n := 0; n < len(in.Sig)/320+200; n++ {
		for ev < len(in.Ev) && in.Ev[ev][0] <= tick { i := int(in.Ev[ev][1]); p.Push(uint32(i*960), in.Sig[i*960:(i+1)*960], s); ev++ }
		f, _ := q.take(320); out = append(out, f...); tick += 20
		if ev >= len(in.Ev) && q.depth() == 0 { break }
	}
	o, _ := json.Marshal(out); os.WriteFile("out.json", o, 0644)
}''')
    r = subprocess.run(["go", "test", "-count=1", "."], cwd=d, capture_output=True, text=True,
                       env=dict(os.environ, GOTOOLCHAIN="local"))
    if r.returncode:
        sys.exit(r.stdout + r.stderr)
    go = np.array(json.load(open(f"{d}/out.json")), np.int16)
    # python, same loop
    q, mp, ev, out = WadJitter(), MeowPlayout(), 0, []
    events = sorted(trace)
    tick = events[0][0] + 7
    sink = lambda pcm: q.push(f32to16(np.asarray(pcm, np.float32)))
    sig32 = sig.astype(np.float32)
    for _ in range(len(sig) // 320 + 200):
        while ev < len(events) and events[ev][0] <= tick:
            i = events[ev][1]
            mp.push(i * WA_FRAME, sig32[i * WA_FRAME:(i + 1) * WA_FRAME], sink)
            ev += 1
        f, _ = q.take(320)
        out.append(f)
        tick += 20
        if ev >= len(events) and len(q.buf) == 0:
            break
    py = np.concatenate(out)
    same = len(go) == len(py) and np.array_equal(go, py)
    print(f"Go {len(go)} samples, Python {len(py)} samples: {'IDENTICAL' if same else 'DIFFERENT'}")
    if not same:
        m = min(len(go), len(py))
        print("  first difference at sample", int(np.argmax(go[:m] != py[:m])))


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--secs", type=float, default=30)
    ap.add_argument("--calls", type=int, default=5, help="independent calls to average")
    ap.add_argument("--wav")
    ap.add_argument("--verify", action="store_true")
    for k in ("wa-loss", "wa-jitter", "wa-reorder", "phone-loss", "phone-jitter"):
        ap.add_argument("--" + k, type=float)
    a = ap.parse_args()
    if a.verify:
        verify()
        sys.exit()
    custom = {k.replace("-", "_"): getattr(a, k.replace("-", "_")) for k in
              ("wa-loss", "wa-jitter", "wa-reorder", "phone-loss", "phone-jitter")}
    if any(v is not None for v in custom.values()):
        base = dict(SCENARIOS["typical"])
        base.update({k: v for k, v in custom.items() if v is not None})
        run("measured", base, a.secs, list(range(1, a.calls + 1)), a.wav)
    else:
        for name, net in SCENARIOS.items():
            run(name, net, a.secs, list(range(1, a.calls + 1)), a.wav)
