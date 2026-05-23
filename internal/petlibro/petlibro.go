// Package petlibro registers the `petlibro://` source URL scheme with
// go2rtc's stream router.  Unlike Wyze/Tapo we do NOT need a cloud
// account: the camera is reached directly over LAN by UID + IP.
//
// Example go2rtc.yaml:
//
//	streams:
//	  granary:   petlibro://192.168.1.66?uid=HG2SYGTKEP4HG3CA111A&audio=true
//	  underbench: petlibro://192.168.1.84?uid=26JFLE2XWUXWU39S111A&quality=sd
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
