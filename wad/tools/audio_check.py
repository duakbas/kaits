#!/usr/bin/env python3
"""
audio_check.py — call-audio quality checker for the KaiOS WhatsApp client.

Modes
  python3 audio_check.py server                     # simulate wad's call audio path and grade it
  python3 audio_check.py make  test.wav             # generate a test signal
  python3 audio_check.py check rec.wav              # analyze a recording alone
  python3 audio_check.py check rec.wav --ref test.wav   # also compare to the test signal

Any format ffmpeg can read works (wav, ogg, opus, m4a, ...).
Deps: numpy, scipy   (pip3 install numpy scipy)   + ffmpeg for non-wav input
Optional: pesq       (pip3 install pesq) -> MOS score when --ref is given
"""
import argparse, os, shutil, subprocess, sys, tempfile
import numpy as np
from scipy.io import wavfile
from scipy import signal

SR_TEST = 48000


# ---------- io ----------
def load(path):
    if not path.lower().endswith(".wav"):
        if not shutil.which("ffmpeg"):
            sys.exit("non-wav input needs ffmpeg (brew install ffmpeg)")
        tmp = tempfile.NamedTemporaryFile(suffix=".wav", delete=False).name
        subprocess.run(["ffmpeg", "-y", "-loglevel", "error", "-i", path, tmp], check=True)
        path = tmp
    sr, x = wavfile.read(path)
    if x.dtype == np.int16:
        x = x / 32768.0
    elif x.dtype == np.int32:
        x = x / 2147483648.0
    elif x.dtype == np.uint8:
        x = (x - 128) / 128.0
    x = x.astype(np.float64)
    if x.ndim > 1:
        x = x.mean(axis=1)
    return sr, x


def db(v):
    return 20 * np.log10(np.maximum(v, 1e-12))


# ---------- test signal ----------
def make_test(path):
    sr = SR_TEST
    parts = []
    sil = lambda s: np.zeros(int(sr * s))
    parts.append(sil(1.0))
    # log sweep 50 Hz -> 20 kHz, 6 s  (bandwidth / frequency response)
    t = np.arange(int(sr * 6)) / sr
    parts.append(0.5 * signal.chirp(t, 50, t[-1], 20000, method="logarithmic"))
    parts.append(sil(1.0))
    # steady 1 kHz tone, 4 s  (clicks/dropouts are obvious on a pure tone)
    t = np.arange(int(sr * 4)) / sr
    parts.append(0.4 * np.sin(2 * np.pi * 1000 * t))
    parts.append(sil(1.0))
    # speech-like bursts: modulated pink noise, 5 s  (codec + noise suppression behaviour)
    n = int(sr * 5)
    w = np.random.default_rng(0).standard_normal(n)
    b, a = [0.049922, -0.095993, 0.050613, -0.004408], [1, -2.494956, 2.017265, -0.522189]
    pink = signal.lfilter(b, a, w)
    pink /= np.abs(pink).max()
    env = (np.sin(2 * np.pi * 3 * np.arange(n) / sr) > 0).astype(float)
    env = signal.lfilter([1], [1, -0.995], env) * 0.005 / 0.005
    parts.append(0.4 * pink * np.clip(env, 0, 1))
    parts.append(sil(1.0))
    y = np.concatenate(parts)
    # fade edges
    f = int(0.005 * sr)
    y[:f] *= np.linspace(0, 1, f)
    y[-f:] *= np.linspace(1, 0, f)
    wavfile.write(path, sr, (y * 32767).astype(np.int16))
    print(f"wrote {path}  ({len(y)/sr:.1f} s, {sr} Hz)")
    print("sections: 1s silence | 6s sweep | 1s | 4s 1kHz tone | 1s | 5s bursts | 1s")


# ---------- analysis ----------
def frames_rms(x, sr, ms=20):
    n = int(sr * ms / 1000)
    k = len(x) // n
    return np.sqrt((x[: k * n].reshape(k, n) ** 2).mean(axis=1)), n


def bandwidth(x, sr):
    f, p = signal.welch(x, sr, nperseg=4096)
    p_db = 10 * np.log10(np.maximum(p, 1e-20))
    peak = p_db[f > 100].max()
    above = np.where(p_db > peak - 40)[0]
    return f[above[-1]] if len(above) else 0.0


def find_clicks(x, sr):
    """Hard cuts: the 1 ms level collapses from audible to (near) silence, or
    jumps back, between two neighbouring milliseconds. That is the signature of
    zero-filled frames — a starved jitter queue, a lost packet padded with
    silence — and it is what you hear as a pop at each edge of the gap.
    Ordinary speech decays over tens of ms, so it doesn't trigger this.
    """
    n = max(1, int(sr * 0.001))
    k = len(x) // n
    lv = db(np.sqrt((x[: k * n].reshape(k, n) ** 2).mean(axis=1)))
    loud, quiet = lv > -45, lv < -70
    edge = (loud[:-1] & quiet[1:]) | (quiet[:-1] & loud[1:])
    idx = (np.where(edge)[0] + 1) * n
    # merge hits within 2 ms
    clicks = []
    for i in idx:
        if not clicks or i - clicks[-1] > sr * 0.002:
            clicks.append(i)
    return np.array(clicks) / sr


def find_dropouts(x, sr, active):
    """Runs of (near) digital silence >= 5 ms inside active audio."""
    n = int(sr * 0.005)
    silent = np.abs(x) < 1e-4
    out, run = [], 0
    for i, s in enumerate(silent):
        if s:
            run += 1
        else:
            if run >= n and active(i / sr):
                out.append(((i - run) / sr, run / sr * 1000))
            run = 0
    return out


def periodicity(times):
    if len(times) < 4:
        return None
    gaps = np.diff(times) * 1000
    med = np.median(gaps)
    # how many gaps are (near) multiples of the median gap
    ratio = gaps / med
    regular = np.mean(np.abs(ratio - np.round(ratio)) < 0.1)
    return med, regular


def check(path, ref_path=None):
    sr, x = load(path)
    dur = len(x) / sr
    print(f"\n== {os.path.basename(path)} ==")
    print(f"format      : {sr} Hz, {dur:.1f} s")

    peak = np.abs(x).max()
    rms, n = frames_rms(x, sr)
    rms_db = db(rms)
    clip = np.mean(np.abs(x) > 0.99) * 100
    print(f"level       : peak {db(peak):.1f} dBFS, DC offset {x.mean():+.4f}")
    print(f"clipping    : {clip:.3f} % of samples" + ("   <-- too hot / AGC overdriving" if clip > 0.01 else ""))

    noise = np.percentile(rms_db, 10)
    speech = np.percentile(rms_db, 95)
    print(f"noise floor : {noise:.1f} dBFS   active level: {speech:.1f} dBFS   => SNR ~{speech - noise:.0f} dB"
          + ("   <-- low" if speech - noise < 30 else ""))

    bw = bandwidth(x, sr)
    cls = "narrowband (phone-like)" if bw < 4500 else "wideband" if bw < 8500 else "super-wideband"
    print(f"bandwidth   : ~{bw/1000:.1f} kHz  => {cls}")

    act_thr = noise + 0.5 * (speech - noise)
    active_frames = rms_db > act_thr
    def active(t):
        i = int(t * sr / n)
        lo, hi = max(i - 5, 0), min(i + 5, len(active_frames))
        return active_frames[lo:hi].any()

    clicks = find_clicks(x, sr)
    print(f"clicks/pops : {len(clicks)}  ({len(clicks)/dur*60:.0f}/min)")
    if len(clicks):
        print("              at " + ", ".join(f"{t:.2f}s" for t in clicks[:12]) + (" ..." if len(clicks) > 12 else ""))
        p = periodicity(clicks)
        if p and p[1] > 0.6:
            print(f"              REGULAR spacing ~{p[0]:.0f} ms  => matches a buffer/chunk size: "
                  "playback scheduling gap or chunk-boundary discontinuity")
        elif p:
            print("              irregular spacing  => network jitter / buffer underruns")

    drops = find_dropouts(x, sr, active)
    tot = sum(d for _, d in drops)
    print(f"dropouts    : {len(drops)}  (total {tot:.0f} ms of digital silence inside audio)")
    if drops:
        print("              " + ", ".join(f"{t:.2f}s/{d:.0f}ms" for t, d in drops[:10]))

    res = {"clicks": len(clicks), "drop_ms": tot, "bw": bw}
    if ref_path:
        res.update(compare(sr, x, *load(ref_path)))
    return res


def compare(sr, x, sr_r, r):
    if sr_r != sr:
        r = signal.resample_poly(r, sr, sr_r)
    # align
    a, b = x / (np.std(x) + 1e-12), r / (np.std(r) + 1e-12)
    c = signal.fftconvolve(a, b[::-1], mode="full")
    lag = np.argmax(np.abs(c)) - (len(b) - 1)
    print(f"\n-- vs reference --")
    print(f"offset      : {lag/sr*1000:+.0f} ms (recording vs ref start)")
    if lag >= 0:
        xa, ra = x[lag:], r
    else:
        xa, ra = x, r[-lag:]
    m = min(len(xa), len(ra))
    xa, ra = xa[:m], ra[:m]
    g = np.dot(xa, ra) / (np.dot(ra, ra) + 1e-12)
    err = xa - g * ra
    print(f"gain        : {db(abs(g)):+.1f} dB")

    # segmental SNR on active frames
    n = int(sr * 0.02)
    k = m // n
    rs = (g * ra[: k * n]).reshape(k, n)
    es = err[: k * n].reshape(k, n)
    pr, pe = (rs ** 2).sum(1), (es ** 2).sum(1)
    act = pr > np.percentile(pr, 50)
    seg = np.clip(10 * np.log10(pr[act] / (pe[act] + 1e-12)), -10, 35)
    print(f"seg. SNR    : {seg.mean():.1f} dB   (>15 good, <5 heavily degraded)")
    res = {"segsnr": seg.mean()}

    try:
        from pesq import pesq
        x16 = signal.resample_poly(xa, 16000, sr)
        r16 = signal.resample_poly(ra, 16000, sr)
        mos = pesq(16000, r16, x16, "wb")
        verdict = "good" if mos > 3.5 else "ok" if mos > 2.5 else "bad"
        print(f"PESQ-WB MOS : {mos:.2f}  ({verdict}; 4.5 = perfect, GSM call ~3)")
        res["mos"] = mos
    except ImportError:
        print("PESQ        : pip3 install pesq  for a MOS score")
    except Exception as e:
        print(f"PESQ        : failed ({e})")
    return res


def server(wad_dir, out_dir):
    """Run the Go simulation (internal/calls/audio_sim_test.go) and grade every output."""
    if not shutil.which("go"):
        sys.exit("needs Go on PATH")
    out_dir = os.path.abspath(out_dir)
    env = dict(os.environ, WAD_AUDIO_SIM=out_dir)
    r = subprocess.run(["go", "test", "-run", "TestAudioSim", "-count=1", "-v", "./internal/calls"],
                       cwd=wad_dir, env=env, capture_output=True, text=True)
    sim = [l.strip() for l in r.stdout.splitlines() if "ticks" in l]
    if r.returncode != 0 or not sim:
        sys.exit(r.stdout + r.stderr)
    print("== simulation (wad's own counters) ==")
    for l in sim:
        print("  " + l.split(": ", 1)[-1])

    ref = os.path.join(out_dir, "ref_16k.wav")
    rows = []
    for f in ["up.wav"] + sorted(f for f in os.listdir(out_dir) if f.startswith("down_")):
        res = check(os.path.join(out_dir, f), ref)
        rows.append((f[:-4], res))

    print("\n== summary ==")
    print(f"  {'path':<14}{'pops':>6}{'gaps ms':>9}{'band kHz':>10}{'segSNR':>8}{'MOS':>6}")
    for name, res in rows:
        mos = f"{res['mos']:.2f}" if "mos" in res else "-"
        print(f"  {name:<14}{res['clicks']:>6}{res['drop_ms']:>9.0f}{res['bw']/1000:>10.1f}"
              f"{res['segsnr']:>8.1f}{mos:>6}")
    print(f"\nWAVs to listen to: {out_dir}")


if __name__ == "__main__":
    here = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("server")
    s.add_argument("--wad", default=os.path.dirname(here), help="wad module dir")
    s.add_argument("--out", default="audio_sim_out")
    m = sub.add_parser("make"); m.add_argument("out")
    c = sub.add_parser("check"); c.add_argument("rec"); c.add_argument("--ref")
    a = ap.parse_args()
    if a.cmd == "server":
        server(a.wad, a.out)
    elif a.cmd == "make":
        make_test(a.out)
    else:
        check(a.rec, a.ref)
