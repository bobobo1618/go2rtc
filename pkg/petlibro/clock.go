package petlibro

import "slices"

// videoClock learns a robust interval from adjacent valid camera timestamps.
// An interpolated run is excluded from sampling. uint32 subtraction preserves
// the camera's millisecond rollover and the RTP timestamp rollover.
type videoClock struct {
	samples    []uint32
	anchor     uint32
	lastCamera uint32
	haveCamera bool
	missing    bool
	last       uint32
	started    bool
}

func (v *videoClock) interval() uint32 {
	if len(v.samples) == 0 {
		return 3600
	} // startup prior only, replaced by camera samples
	s := append([]uint32(nil), v.samples...)
	slices.Sort(s)
	return s[len(s)/2]
}
func (v *videoClock) timestamp(camera uint32, valid bool) (uint32, bool) {
	step := v.interval()
	interpolated := !valid
	if valid && v.haveCamera && !v.missing {
		delta := camera - v.lastCamera
		if delta >= 10 && delta <= 200 {
			v.samples = append(v.samples, delta*90)
			if len(v.samples) > 31 {
				v.samples = v.samples[1:]
			}
			step = v.interval()
		}
	}
	var pts uint32
	if v.started {
		pts = v.last + step
	}
	if valid && v.haveCamera {
		delta := int32(camera - v.lastCamera)
		// Advance by valid camera time, less intervals already interpolated.
		if delta > 0 && delta < 10000 {
			candidate := v.cameraPTS() + uint32(delta)*90
			if !v.started || int32(candidate-v.last) >= int32(step/4) {
				pts = candidate
			} else {
				interpolated = true
			}
		} else {
			interpolated = true
		}
	}
	if valid {
		v.lastCamera = camera
		v.haveCamera = true
		v.anchor = pts
	}
	v.missing = !valid
	v.last = pts
	v.started = true
	return pts, interpolated
}
func (v *videoClock) cameraPTS() uint32 { return v.anchor }
