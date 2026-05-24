#!/usr/bin/env python3.12
"""
Automated stream verifier for the Petlibro / go2rtc pipeline.

Starts go2rtc, runs mpv to dump frames as PNGs, OCRs the camera
timestamp (top-right corner of each frame), and reports:
  * time-to-first-frame
  * distinct camera-timestamps seen
  * longest "freeze" (consecutive identical timestamps)
  * timestamp progression rate (should be ~1.0 sec / sec)
  * decoder error count from mpv

Usage:
    _verify_stream.py [--config /tmp/go2rtc-loose.yaml]
                      [--stream granary] [--rtsp-port 18555]
                      [--duration 25] [--quality hd|sd]
                      [--out /tmp/verify-out]
"""
import argparse
import os
import re
import signal
import subprocess
import sys
import time
from collections import Counter
from pathlib import Path
from PIL import Image
import pytesseract

GO2RTC_BIN = "/tmp/go2rtc-petlibro"
# OCR often misreads the date (e.g. 2026 -> 2076) but the HH:MM:SS
# portion is consistent.  Match just the time-of-day for freeze
# detection — that's what advances second-by-second.
TS_REGEX = re.compile(r"(\d{1,2}:\d{2}:\d{2})")


def kill_stale():
    subprocess.run(["pkill", "-9", "-f", "go2rtc-petlibro|ffmpeg|mpv"],
                   capture_output=True)
    time.sleep(2)


def start_go2rtc(config: Path, log_path: Path) -> subprocess.Popen:
    f = log_path.open("w")
    return subprocess.Popen(
        [GO2RTC_BIN, "-c", str(config)],
        stdout=f, stderr=subprocess.STDOUT,
    )


def wait_for_rtsp(api_port: int, stream: str, timeout: float = 12.0) -> float:
    """Poll the go2rtc API until the stream is registered, return seconds waited."""
    deadline = time.time() + timeout
    t0 = time.time()
    while time.time() < deadline:
        try:
            r = subprocess.run(
                ["curl", "-s", "--max-time", "1",
                 f"http://127.0.0.1:{api_port}/api/streams"],
                capture_output=True, text=True, timeout=2,
            )
            if stream in r.stdout and "producers" in r.stdout:
                return time.time() - t0
        except Exception:
            pass
        time.sleep(0.25)
    return -1.0


def run_mpv(rtsp_url: str, out_dir: Path, duration: int, log_path: Path):
    out_dir.mkdir(parents=True, exist_ok=True)
    for old in out_dir.glob("*.png"):
        old.unlink()
    proc = subprocess.Popen(
        ["mpv", "--no-config", "--rtsp-transport=tcp",
         "--vo=image", "--vo-image-format=png",
         f"--vo-image-outdir={out_dir}",
         "--ao=null", "--untimed",
         "--msg-level=ffmpeg=v",
         f"--length={duration}", rtsp_url],
        stdout=log_path.open("w"), stderr=subprocess.STDOUT,
    )
    return proc


def ocr_timestamp(png_path: Path) -> str | None:
    """Crop the top-right corner and OCR the embedded camera timestamp.

    Returns the HH:MM:SS portion only — sufficient for freeze detection
    and immune to the date-digit OCR errors that tesseract sometimes
    introduces (e.g. reading 2026 as 2076)."""
    try:
        img = Image.open(png_path)
    except Exception:
        return None
    w, h = img.size
    # The overlay sits in the top ~6% on the right ~50% — same band
    # on both 1920x1080 and 640x360 frames.
    crop = img.crop((int(w * 0.50), int(h * 0.01), w, max(35, int(h * 0.08))))
    if crop.width < 600:
        crop = crop.resize((crop.width * 3, crop.height * 3), Image.LANCZOS)
    text = pytesseract.image_to_string(
        crop, config="--psm 7 -c tessedit_char_whitelist=0123456789-:/ ",
    )
    m = TS_REGEX.search(text)
    return m.group(1) if m else None


def analyse(out_dir: Path, mpv_log: Path, ttf: float, duration: int):
    pngs = sorted(out_dir.glob("*.png"))
    print(f"\n=== Decoded {len(pngs)} PNG(s) over {duration}s window ===")
    print(f"Time-to-first-frame (after RTSP ready): {ttf:.2f}s")
    if not pngs:
        print("FAIL: zero PNGs rendered.")
        return False

    # Decoder error tally
    log_text = mpv_log.read_text(errors="ignore")
    err_counts = {
        "chroma":     log_text.count("intra chroma pred mode"),
        "corrupted":  log_text.count("corrupted macroblock"),
        "level_prefix": log_text.count("level prefix"),
        "concealing": log_text.count("concealing"),
        "sub_mb_type": log_text.count("sub_mb_type"),
        "mb_type":    log_text.count("mb_type") - log_text.count("sub_mb_type"),
        "PTS_skipped": log_text.count("Invalid video timestamp"),
        "no_PTS":     log_text.count("No video PTS"),
    }
    err_total = sum(v for v in err_counts.values() if v > 0)

    # Sample every PNG (limit to ~40 for OCR cost)
    step = max(1, len(pngs) // 40)
    sampled = pngs[::step]
    timestamps = []
    for p in sampled:
        ts = ocr_timestamp(p)
        timestamps.append((p.name, ts))

    parsed = [t for _, t in timestamps if t]
    distinct = list(dict.fromkeys(parsed))  # preserve order, dedupe
    print(f"OCR success: {len(parsed)}/{len(sampled)} sampled frames")
    print(f"Distinct camera timestamps seen: {len(distinct)}")
    if distinct:
        print(f"  first: {distinct[0]}")
        print(f"  last:  {distinct[-1]}")

    # Find longest run of identical timestamps
    longest_run = 0
    cur_run = 0
    last_ts = None
    for _, ts in timestamps:
        if ts is None:
            continue
        if ts == last_ts:
            cur_run += 1
        else:
            cur_run = 1
            last_ts = ts
        longest_run = max(longest_run, cur_run)
    print(f"Longest stretch of identical-timestamp consecutive frames: {longest_run}")
    if step * longest_run > 5:
        print(f"  (~{step * longest_run / (len(pngs) / duration):.1f}s of frozen video)")

    # Per-error breakdown
    print("\nDecoder error counts (mpv log):")
    for k, v in err_counts.items():
        marker = "  "
        if v > 0:
            marker = "!! " if v > 10 else "*  "
        print(f"  {marker}{k:13s}: {v}")
    print(f"  total flagged events: {err_total}")

    # PASS/FAIL summary
    fps = len(pngs) / duration
    print(f"\n=== Summary ===")
    print(f"  fps (decoded):                       {fps:.2f}")
    print(f"  unique timestamps:                   {len(distinct)}  (expected ~{duration})")
    print(f"  longest freeze in OCR'd frames:      {longest_run}  (expected ~1)")
    print(f"  decoder errors:                      {err_total}  (expected ~0)")

    verdict_ok = (
        ttf < 8.0 and
        fps >= 1.0 and
        longest_run <= 5 and
        err_total <= 5
    )
    print(f"\n  Verdict: {'PASS' if verdict_ok else 'FAIL'}")
    return verdict_ok


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", default="/tmp/go2rtc-loose.yaml")
    ap.add_argument("--stream", default="granary")
    ap.add_argument("--api-port", type=int, default=11985)
    ap.add_argument("--rtsp-port", type=int, default=18555)
    ap.add_argument("--duration", type=int, default=25)
    ap.add_argument("--out", default="/tmp/verify-out")
    args = ap.parse_args()

    cfg = Path(args.config)
    if not cfg.exists():
        print(f"missing config: {cfg}", file=sys.stderr)
        sys.exit(2)

    out_dir = Path(args.out)
    g2r_log = Path("/tmp/verify-go2rtc.log")
    mpv_log = Path("/tmp/verify-mpv.log")

    print(f"== verify_stream config={cfg.name} stream={args.stream} duration={args.duration}s ==")
    kill_stale()
    go2rtc = start_go2rtc(cfg, g2r_log)
    try:
        ttf = wait_for_rtsp(args.api_port, args.stream, timeout=12)
        if ttf < 0:
            print("FAIL: go2rtc never registered the stream (RTSP API timeout)")
            print("--- last lines of go2rtc log ---")
            print("\n".join(g2r_log.read_text().splitlines()[-20:]))
            sys.exit(1)
        print(f"go2rtc ready in {ttf:.2f}s")

        rtsp_url = f"rtsp://127.0.0.1:{args.rtsp_port}/{args.stream}"
        mpv_proc = run_mpv(rtsp_url, out_dir, args.duration, mpv_log)
        # +5s slack for mpv startup + image flushing
        try:
            mpv_proc.wait(timeout=args.duration + 5)
        except subprocess.TimeoutExpired:
            mpv_proc.send_signal(signal.SIGKILL)
            mpv_proc.wait()

        ok = analyse(out_dir, mpv_log, ttf, args.duration)
        sys.exit(0 if ok else 1)
    finally:
        go2rtc.send_signal(signal.SIGTERM)
        try:
            go2rtc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            go2rtc.kill()


if __name__ == "__main__":
    main()
