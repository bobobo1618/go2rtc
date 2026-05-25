// Package petlibro registers the `petlibro://` source URL scheme with
// go2rtc's stream router.  Unlike Wyze/Tapo we do NOT need a cloud
// account: the camera is reached directly over LAN by UID + IP.
//
// Example go2rtc.yaml:
//
//	streams:
//	  cam1: petlibro://<camera-ip>?uid=<20-char-UID>&audio=true
//	  cam2: petlibro://<camera-ip>?uid=<20-char-UID>&quality=sd
//
// The UID is the 20-character identifier printed on the camera's
// back-label or visible in the Petlibro app under camera details.
//
// HD/SD-sticky caveat: the `quality=hd|sd` query parameter SELECTS
// which already-enabled stream the camera should emit, it does NOT
// turn streams on.  HD/SD-on/off is persisted in the Petlibro cloud
// config (set via the Petlibro phone app) and survives reboots; if a
// camera was set to "HD only" via the app, `?quality=sd` will get you
// nothing.  See internal/petlibro/README.md for the full table.
package petlibro

import (
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/petlibro"
)

func Init() {
	log := app.GetLogger("petlibro")
	// Inject the zerolog instance into the library so its diagnostic
	// messages reach the same sink as the rest of go2rtc.  The pkg/
	// side defaults to a Nop logger so unit tests don't depend on
	// internal/ being loaded.
	petlibro.SetLogger(log)
	streams.HandleFunc("petlibro", func(rawURL string) (core.Producer, error) {
		log.Debug().Msgf("petlibro: dial %s", rawURL)
		return petlibro.NewProducer(rawURL)
	})
}
