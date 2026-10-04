# PETLIBRO LAN camera support

This fork's authoritative V2 documentation is in
[PETLIBRO PLAF203 V2 technical notes](../../docs/petlibro-plaf203-v2.md).

Status: **BETA / EXPERIMENTAL**. Validated on one PLAF203, firmware 3.1.54,
hardware 1.0.7; other firmware and models are unverified.

See the technical notes for configuration, HD/SD H264 and AAC, transport details,
complete-only IDR recovery, independent ACK/playout state, FEC validation,
measured results, normal reconnect and runtime codec-change limitations.

The original integration was developed in
[bobobo1618/go2rtc](https://github.com/bobobo1618/go2rtc) on top of
[AlexxIT/go2rtc](https://github.com/AlexxIT/go2rtc).

Earlier integration notes described emitting damaged IDRs, one-tick timestamp
fallbacks, and speculative UID/model guidance. Those descriptions do not apply
to this V2 publication. Device UIDs are unique; UID-based LAN IP discovery
requires an already-known UID and does not extract it.
