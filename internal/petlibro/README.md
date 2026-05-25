# Petlibro

Clean-room LAN P2P client for [Petlibro](https://petlibro.com/) pet
cameras using the camera's Kalay/TUTK-family UDP protocol on port
32761. No cloud account, no Petlibro app, no vendor SDK is required at
runtime; the only cloud dependency is the *initial* camera provisioning
(which you do once via the Petlibro app on your phone) and any
firmware-side authentication gate that camera does against the Petlibro
cloud whitelist.

**Features:**

- H.264 video (HD / SD, sticky — see "Stream quality" below)
- AAC audio (`audio=true`)
- Pure-Go, no CGO, no native SDK

**Known limitations:**

- No two-way audio / backchannel (see [Roadmap](#roadmap))
- Stream-quality switching is **not** per-session — see "Stream quality"
- One model verified (PLAF203); other PLAF variants expected to work
  but untested — see [Supported models](#supported-models)

## Stream URL format

```
petlibro://[IP]?uid=[UID][&audio=true][&quality=hd|sd][&strict=1][&verbose=1]
```

| Parameter | Required | Default | Description                                                     |
|-----------|----------|---------|-----------------------------------------------------------------|
| `IP`      | yes      | —       | Camera's local IP address (port defaults to `32761`)            |
| `uid`     | yes      | —       | 20-character TUTK camera UID                                    |
| `audio`   | no       | `false` | Enable AAC audio (`true` or `1`)                                |
| `quality` | no       | `hd`    | Pick `hd` or `sd` from the camera's already-enabled streams     |
| `strict`  | no       | `false` | Drop any IDR with fragment loss instead of emitting it gapped   |
| `verbose` | no       | `false` | Periodic stream-health stats + handshake trace to stdout        |

**Example go2rtc.yaml:**

```yaml
streams:
  petfeeder_hd: petlibro://192.168.1.42?uid=PLAF20300000000ABCD0&audio=true
  petfeeder_sd: petlibro://192.168.1.42?uid=PLAF20300000000ABCD0&quality=sd
```

## How to find the UID

The 20-character UID is printed on a label on the back/bottom of the
camera (under or near the QR code used for app-pairing). You can also
find it in the Petlibro app under camera details — most Petlibro
PLAF-series cameras format the UID as a printable ASCII string starting
with the model prefix (e.g. `PLAF203...`).

The camera's local IP can be discovered from your router's DHCP table
or by running a LAN scan; the camera advertises its presence over the
Kalay LAN_SEARCH protocol on UDP/32761 but go2rtc requires you to
already know the IP.

## Stream quality

> [!IMPORTANT]
> Petlibro cameras run with one or both of their HD (1920×1080, main)
> and SD (640×360, sub) streams *pre-enabled* in their firmware's
> cloud-side stream configuration. The `quality=` URL parameter picks
> which of the **already-enabled** streams go2rtc keeps; it does NOT
> turn streams on or off at the camera.

If you set `quality=sd` but the camera was provisioned HD-only by the
Petlibro app, no frames will arrive. To change which streams the camera
publishes you have to open the Petlibro app and toggle them there — the
setting is sticky in the cloud config and persists across reboots and
go2rtc restarts.

When both streams are enabled, the camera dual-streams them on the
same UDP channels and go2rtc filters by the trailer-borne stream-id
byte (`0x01 = HD`, `0x02 = SD`). See `pkg/petlibro/templates.go`
`qualityHD` / `qualitySD` and `pkg/petlibro/client.go`
`stripFragmentMetadataTrailer` for the wire-level details.

## Strict mode

`strict=1` flips the lossy-network policy:

| Mode             | On IDR with lost fragment | On P-frame with lost fragment |
|------------------|---------------------------|-------------------------------|
| Default          | emit gapped (artefacts)   | drop                          |
| `strict=1`       | drop + poison GOP         | drop                          |

Default mode trades localised macroblock artefacts at the gap for video
fluency. Strict mode trades a multi-second freeze (until the next clean
IDR) for pristine pixels. Use strict when downstream decoder warnings
are louder than freezes are.

## Supported models

Verified working (one author with one camera):

| Model    | Firmware tested            | Notes                              |
|----------|---------------------------|-------------------------------------|
| PLAF203  | factory-shipped 2024-2025 | HD-only and HD+SD dual-stream both ok |

Expected to work but **untested** (same TUTK-firmware family, same
LOGIN body bytes per cloud-RE analysis, same bootstrap sequence):

- PLAF103
- Other PLAF-prefix PLAF-series cameras
- Other Petlibro cameras built on the same Kalay/TUTK firmware

If you try this against another model, please open an issue (or send a
PR adding your model + firmware version to this table) — the most
likely failure mode is a small divergence in the IOCtrl bootstrap
sequence that's trivial to fix once observed.

## Roadmap

- Two-way audio (backchannel to camera speaker) — **out of scope** for
  the initial PR. PLAF cameras have a speaker and the protocol family
  supports a backchannel (cf. `pkg/wyze/backchannel.go`,
  `pkg/tapo/backchannel.go`), but the Petlibro-specific wire bytes
  haven't been captured. Future work.

## Troubleshooting

**`petlibro: LOGIN_RESP timeout`** within 5 s of starting the stream:
the camera received LOGIN A+B but never acknowledged. Most likely
causes (in observed-frequency order):

1. **Camera was never provisioned via the Petlibro app on this LAN.**
   The camera maintains a long-lived cloud link back to Petlibro
   infrastructure (Kalay/Nebula whitelist gate) and refuses LAN
   sessions from peers whose presence hasn't been signalled through
   that channel. Open the Petlibro app once on a phone on the same
   LAN — that's enough to flip the gate. The app does NOT need to
   stay running afterwards.
2. **Wrong UID.** The camera silently ignores LOGIN bytes whose
   embedded `view_account` doesn't resolve to a known camera. Verify
   the 20-character UID matches the label.
3. **Wrong IP.** `petlibro://` doesn't do mDNS — you must supply the
   camera's current LAN IP. Re-check after a router reboot if DHCP
   reassigned.
4. **Camera offline / unreachable.** Standard ICMP/UDP-32761 reachability.

The `verbose=1` URL parameter enables handshake tracing to stdout so
you can see how far the LAN_SEARCH3 → KNOCK2 → LOGIN A/B sequence
got before the timeout.

**Video starts but with corrupted macroblocks at the bottom strip:**
the IDR's end fragment is being lost on your network and you're seeing
the default "emit gapped IDR" recovery path. Either fix the network
(usually 2.4 GHz wifi congestion) or enable `strict=1` to drop gapped
IDRs entirely.

**`ffmpeg` reports `Invalid video timestamp X -> X` warnings:** harmless
PTS-bumping near forceDrain flushes. The PTS bumper guarantees strictly
monotonic output but can produce nano-deltas when several AUs flush
inside one camera-clock millisecond.

## Wire protocol references

The protocol bytes were reverse-engineered from PCAPdroid captures of
the official Petlibro Android app talking to PLAF103 / PLAF203 cameras.
Key constants:

- **LOGIN A/B bodies** (570 / 572 B) are byte-exact templates with
  only the 4-byte `login_serial` at body+0x14 varying per attempt.
  LOGIN B's serial is always LOGIN A's + 1. The encrypted region
  begins at body+0x24 and carries `view_account="admin"`,
  `view_password="888888"`, an opcode-support bitmap, and a few flag
  bytes; encryption is the same Luffy / TUTK `ReverseTransCodePartial`
  used by Wyze. See `pkg/petlibro/templates.go` and
  `pkg/petlibro/templates_test.go` (byte-exact regression fixture).
- **Bootstrap sequence**: LAN_SEARCH3 → KNOCK2 → LOGIN A → LOGIN B →
  5-IOCtrl-cmd bootstrap (SETSTREAMCTRL, vendor 0x0372,
  GET_AUDIO_OUT_FORMAT, GET_FORMAT, IPCAM_START) → optional
  AUDIO_ENABLE. See `pkg/petlibro/client.go` `bootstrap()`. The
  official Petlibro app sends a 257-byte DTLS-shaped first packet
  before LOGIN A; we empirically bisected it (post-power-cycle the
  camera responds to LOGIN within 15 ms without it) and dropped it.
  Evidence trail: `.omc/research/_dtls_skip_test/`.
- **Frame reassembly**: per-channel (ch=0x05 main / 0x07 sub /
  0x03 audio); end-fragment detected by 15-byte ms-timestamp trailer
  on the LAST data fragment (signature: codec_id 0x4e + flags +
  stream-id + 7 zero bytes + 4 B LE ms timestamp). See
  `pkg/petlibro/client.go` `stripFragmentMetadataTrailer` and the
  per-channel `channelAsm` state.

