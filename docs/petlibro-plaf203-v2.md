# PETLIBRO PLAF203 V2 technical notes

**Status: BETA / EXPERIMENTAL. Community maintained.** This is a single-device
validation report, not a compatibility guarantee for other cameras or firmware.

## Provenance and scope

Original project: [AlexxIT/go2rtc](https://github.com/AlexxIT/go2rtc).
PETLIBRO implementation: [bobobo1618/go2rtc](https://github.com/bobobo1618/go2rtc),
branches `petlibro-integration` and `petlibro-integration-embedded`.
The exact publication base is
[`8fc5ef560d63dd2f5cf1bf299f18784d05eeba92`](https://github.com/bobobo1618/go2rtc/commit/8fc5ef560d63dd2f5cf1bf299f18784d05eeba92)
("Fix Petlibro producer frame assembly").

This branch selectively carries the PETLIBRO changes from the locally validated
V2 source `f306700dfd4c021b90e87f89634aaad8a7321e19`. That local commit is not
published: unrelated test/compatibility edits and a private benchmark default
were excluded. No unrelated source fix is required for the Windows build.
The transport, FEC, ACK, assembly and clock changes are coupled and are kept in
one implementation commit, with platform support and regression tests separate.

## Tested environment

| Item | Validated device/environment |
|---|---|
| Device | PETLIBRO Granary Smart Camera Feeder PLAF203 |
| Firmware / hardware | 3.1.54 / 1.0.7 |
| SoC / platform | GK7205V200 / GK |
| Linux | 4.9y |
| Video | H264; HD 1920x1080, SD 640x360 |
| Audio | AAC; H264 + AAC confirmed together |
| Transport | LAN UDP, TUTK/Kalay based implementation |

The stock PETLIBRO app and cloud remain available. This method requires no
firmware modification, MQTT hijack, DNS hijack, or cloud disabling. This does
not constitute a long simultaneous-app compatibility or cloud-independence test.

## Why V2 was needed

### Firmware 3.1.54 extended transport header

Observed packet types `0x08`, `0x09`, and `0x0d` carry eight extra transport
bytes. The AV header starts at **24** and media payload at **44**, measured
relative to the decrypted inner message (after the outer 28-byte wrapper).
Legacy offsets are 16 and 36. Reading extension bytes as channel/sequence fields
misclassified valid video, discarded packets, synthesized sequence gaps and
damaged H264 assembly. This was an important root cause on firmware 3.1.54.

`parseDatagram` normalizes the extension before classification and assembly.
It separately preserves the legacy `0x0d` AAC layout; the shared type byte alone
does not identify extended video. Regression tests cover all three types and
both AAC layouts.

### Independent ACK and playout frontiers

Previously `contiguousAckExt()` derived ACK state from `avNextExt - 1`.
`forceDrain()` could skip a missing wire packet and thereby acknowledge data
that had never arrived. Regression example: sequence `0x4001` is absent,
playback advances to `0x4003`, and the old ACK incorrectly becomes `0x4002`.

V2 owns receipt state independently in `recoveryState`; the effective ACK is
published atomically. The regression now holds ACK at `0x4000` while playout
advances. A late arrival can repair the ACK hole without replaying old media.
An explicit timed waiver advances ACK after `ack_grace_ms`; therefore telemetry
`ack` includes counted give-ups and is not an unconditional proof of reception.
Playback skipping never itself waives ACK. ACK-triggered camera retransmission
has not been established on this firmware.

### Validated FEC / parity recovery

Channel `0x0c` carries structured redundancy, not arbitrary unknown media.
The implementation uses systematic GF(256) interpolation with polynomial
`0x11d`. It accepts groups of 2..32 data shards, stores at most 64 groups and
expires groups older than two seconds when processing FEC traffic.

Reconstruction is disabled until parity recomputed from **three complete
groups** matches byte-for-byte in the current session. The development analysis
validated the model against real complete packet groups. Published tests use
synthetic payloads and a small observed sequence/parity fact; private captures
are not included. A complete-group parity mismatch disables reconstruction
for that session. Recovered headers, lengths and sequence ranges are checked
before re-entering the parser. This is validation-gated single-erasure recovery,
not cryptographic integrity checking; multiple missing shards cannot be repaired
with one parity shard. Structural rejection increments `invalid` where applicable.

The live final sample observed 391 validated groups, 8 reconstructed packets and
`invalid=0`. These are observations from that sample, not a universal guarantee.

### Reorder, gaps and IDR/GOP recovery

The fixed high-water/aggressive drain policy is replaced with first-seen gap
timers and independent playback/ACK state. Tested tuning is `reorder_ms=120`,
`idr_grace_ms=350`, `ack_grace_ms=1000`; periodic service has 20 ms granularity.
An expired head gap is skipped while younger gaps retain their own deadlines.

Incomplete IDRs are discarded in both strict modes. Pending P access units can
wait for a complete IDR; dependants of a lost IDR are suppressed until a clean
IDR restores the GOP. `strict=1` additionally poisons the GOP after a damaged
P access unit. `strict=0` still drops damaged P access units and incomplete IDRs.
Transport completeness does not prove that every source bitstream is valid.

### PTS and shutdown

The old `lastEmitTs + 1` fallback is replaced by interpolation using a rolling
median of up to 31 adjacent valid camera deltas (10..200 ms). The startup prior
is 40 ms. Missing/duplicate timestamps advance by the learned nominal interval;
uint32 camera/RTP rollover is covered by tests. Real gaps can still create long
timestamp excursions. Shutdown signals `done` without closing the frame queue
concurrently with a producer send.

### Telemetry

With `verbose=1` and debug logging, actual emitted names include:

- `transport: extendedHeaders`
- `fec: parity`, `validated`, `invalid`, `recovered`
- `recovery: ack`, `playback`, `gaps`, `recovered`, `lateAfterPlayout`,
  `gapTimeout`, `ackGiveups`, `waivedPackets`, `IDRgap`, `IDRrecovered`,
  `IDRdrop`, `Pdrop`, `maxDepth`, `maxAgeMs`, `interpolation`, `nominalMs`
- `arrival: maxUDPGapMs`; ordinary statistics emit `forceDrain`, `frags lost`
  and `qDrops reader=... emit=...`. The evaluation tables call these last
  counters `fragLost`, `readerDrops` and `emitDrops`; those are report labels,
  not all literal log field names.

FEC and recovery `recovered` are different counters. Most V2 counts are interval
deltas; frontiers are current state and depth/age/UDP gap are interval maxima.
`IDRgap` counts IDR-associated timed-out gaps, not decoded picture quality.

## Configuration

Each device has its own unique PETLIBRO UID. Use your device's actual UID.
This publication does not claim a validated method of discovering the UID from
a label or app screen. Existing UID-based LAN discovery locates an IP only
after a UID is already known; it does not discover the UID itself.

```yaml
streams:
  petlibro:
    - "petlibro://<CAMERA_IP>?uid=<PETLIBRO_UID>&audio=true&quality=hd&verbose=1&strict=0&reorder_ms=120&idr_grace_ms=350&ack_grace_ms=1000"
```

| Option | Meaning |
|---|---|
| `quality=hd` / `quality=sd` | Select HD / SD from the camera's enabled streams; not an instruction to enable a firmware stream |
| `audio=true` | Enable AAC (default false) |
| `strict=0` | Default recovery policy described above; does not emit incomplete IDRs |
| `reorder_ms` | Normal gap grace; default 120 ms |
| `idr_grace_ms` | IDR-associated gap grace; default 350 ms |
| `ack_grace_ms` | Independent ACK give-up grace; default 1000 ms |
| `verbose=1` | Enable handshake and periodic diagnostics; use debug log level to see debug messages |

Timing values must be 20..5000 ms and satisfy
`reorder_ms <= idr_grace_ms < ack_grace_ms`.

H264 + AAC:

```text
rtsp://<GO2RTC_HOST>:8554/petlibro?video=h264&audio=aac
```

Video only:

```text
rtsp://<GO2RTC_HOST>:8554/petlibro?video=h264
```

## Build and publication checks

Go 1.24.13, Windows amd64, CGO disabled was used for publication.
Download Go from the [official Go downloads](https://go.dev/dl/).
From a clean standalone clone of `petlibro-plaf203-v2` (with a `.git` directory,
not a linked worktree: this Go toolchain omits VCS stamping for the latter):

```powershell
go version
go test ./pkg/petlibro
go vet ./pkg/petlibro/...
go test ./...
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$env:CGO_ENABLED = '0'
go build -trimpath -buildvcs=true -o go2rtc-petlibro-plaf203-v2-windows-amd64.exe .
./go2rtc-petlibro-plaf203-v2-windows-amd64.exe -version
go version -m ./go2rtc-petlibro-plaf203-v2-windows-amd64.exe
Get-FileHash ./go2rtc-petlibro-plaf203-v2-windows-amd64.exe -Algorithm SHA256
```

The app retains its upstream `1.9.14` version family; its `+dev.<revision>` suffix
and embedded `vcs.revision` identify this fork build. The release tag is the V2
beta identifier. Check `vcs.modified=false` and the full revision against the
release source commit. `-trimpath` removes local build paths.

PETLIBRO tests and vet pass. `go test ./...` on Go 1.24.13 fails in unchanged
upstream packages: `ivideon`/`tuya` nonconstant `fmt.Errorf` vet diagnostics;
`core`, `hap/camera`, `hap/hds` stale test symbols; assertion/fixture failures in
`internal/ffmpeg`, `internal/streams`, `aac`, `creds`, `hap/tlv8`.
These are not repaired or hidden to make the suite appear green. The normal
Windows application build is separate from these unrelated test/vet failures.

## Test methodology and measured results

The measurements below are maintainer-supplied results of earlier tests on the
single device above. Publication checks rerun unit tests, vet, build and CLI
smoke tests; they do not restart production or repeat live camera tests.
Baseline and V2 samples used 90-second captures beginning at an IDR, counted
video frames/keyframes, examined PTS intervals and transport diagnostics, and
decoded H264 through FFmpeg. Example offline decode command:

```text
ffmpeg -v error -i capture.h264 -f null -
```

Zero decoder errors in a short capture do not prove absence of concealed
reference artefacts or establish long-term reliability. Video measurements
and subsequent audio/RTSP validation are separate observations.

| Metric (90 sec) | Baseline | V2 intermediate | V2 final |
|---|---:|---:|---:|
| Frames / keyframes | 2059 / 81 | 2125 / 85 | 2075 / 83 |
| FPS | 22.878 | 23.611 | 23.055 |
| PTS min / median / p95 (ms) | 39 / 40 / 80 | — / — / 40 | 40 / 40 / 40 |
| Maximum observed PTS interval (ms) | 120 | — | 1040 |
| FFmpeg decode error lines | 58 | 0 | 0 |
| Macroblock errors | 29 | — | 0 |
| `fragLost` | 105 | — | 16 |
| `forceDrain` | 181 | 11 | 8 |
| `gaps` | — | 14 | 15 |
| Recovery `recovered` / `gapTimeout` | — | — | 7 / 8 |
| FEC validated / recovered / invalid | — | — / 3 / — | 391 / 8 / 0 |
| Extended headers | — | — | 227 |
| `readerDrops` / `emitDrops` | 0 / 0 | — | 0 / 0 |
| `IDRdrop` | — | — | 5 |

An em dash means not reported, not zero.

Earlier RTSP smoke test at `rtsp://127.0.0.1:8558/petlibro_v2`:
30-second FFmpeg run, exit 0, 730 frames, reported approximately 25.49 fps,
dup=0, drop=0 and zero stderr decode errors. The FPS figure is the reported
FFmpeg value, not 730 divided by the requested wall-clock duration.

Live visual playback started quickly and was generally smooth. Rare glitches
or distortion of approximately one second were observed, correlated with real
UDP arrival gaps / IDR-loss blocks. AAC and simultaneous H264 + AAC were
confirmed. Production-style RTSP selection with `?video=h264&audio=aac` worked.

## Normal reconnect and known beta limitations

A client opened the stream and played successfully, then closed. The upstream
on-demand session was allowed to close; after **more than 75 seconds**, reopening
the client successfully started the stream again. Normal disconnect/reconnect
was not observed to require restarting go2rtc every time.

Changing downstream RTSP codec/source selection during runtime, for example
from `?video=h264` to `?video=h264&audio=aac`, left the existing PETLIBRO/TUTK
session in `probe timeout` in some tests. A clean go2rtc restart restored video
and audio. This is a known beta limitation, not the normal reconnect result.
Choose the intended codec selection before starting a session. Any operational
restart should be a deliberate maintenance action by the operator.

No week-long soak test is complete. Other PLAF203 firmware and other PETLIBRO
models have not been validated. Single parity cannot repair multiple losses;
Wi-Fi loss and IDR gaps can still interrupt playback. No two-way audio is claimed.
Please report device/firmware, codec selection, sanitized counters and a clear
reproduction through this fork's issues.

## Troubleshooting and privacy

- For login or probe timeouts, check the configured UID, IP, LAN reachability
  and existing session/codec selection. UID-based IP discovery is distinct from
  UID discovery. Do not assume label/app instructions for other models apply.
- For freezes or decoder errors, inspect `maxUDPGapMs`, `IDRdrop`, `gapTimeout`,
  FEC validation and queue drops. A larger grace period also increases latency.
- For SD without frames, confirm that the camera actually supplies an SD stream;
  this URL selects media rather than enabling firmware streams.
- Keep camera URLs, UIDs, credentials, captures and raw runtime logs private.
  Debug traces can contain device identifiers and packet data; sanitize reports.
- Use a trusted LAN and authenticated access controls. Do not expose unauthenticated
  go2rtc API/RTSP listeners directly to the Internet. This fork adds no authentication
  guarantee to the underlying camera transport.

## Credits and license

AlexxIT and go2rtc contributors created go2rtc. bobobo1618 and contributors
developed the PETLIBRO integration on which this branch depends. The maintainer
used OpenAI Codex for V2 debugging, implementation, regression tests and analysis;
see [What OpenAI Codex changed](../README.md#what-openai-codex-changed).
AI assistance is not evidence of correctness. This is not an official OpenAI,
Codex or PETLIBRO product and implies no endorsement. The existing
[MIT LICENSE](../LICENSE) is preserved.
