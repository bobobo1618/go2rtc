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
package petlibro

import (
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/petlibro"
)

func Init() {
	log := app.GetLogger("petlibro")
	streams.HandleFunc("petlibro", func(rawURL string) (core.Producer, error) {
		log.Debug().Msgf("petlibro: dial %s", rawURL)
		return petlibro.NewProducer(rawURL)
	})
}
