package player

import (
	"log/slog"

	pulse "github.com/jfreymuth/pulse/proto"
)

const defaultSink = "@DEFAULT_SINK@"

type VolumeInfo struct {
	Volume int
	Mute   bool
}

type PulseMonitor struct {
	client        *pulse.Client
	volumeChan    chan VolumeInfo
	setVolumeChan chan int // -1 for toggle mute, -2 for mute, -3 for unmute, >=0 for level
}

func NewPulseMonitor() (*PulseMonitor, error) {
	client, conn, err := pulse.Connect("")
	if err != nil {
		return nil, err
	}
	_ = conn // conn lifetime managed with client
	return &PulseMonitor{
		client:        client,
		volumeChan:    make(chan VolumeInfo, 8),
		setVolumeChan: make(chan int, 8),
	}, nil
}

func (p *PulseMonitor) SetVolume(level int) {
	if level < 0 {
		level = 0
	} else if level > 100 {
		level = 100
	}
	p.setVolumeChan <- level
}

func (p *PulseMonitor) SetMute(mute bool) {
	if mute {
		p.setVolumeChan <- -2
	} else {
		p.setVolumeChan <- -3
	}
}

func (p *PulseMonitor) ToggleMute() {
	p.setVolumeChan <- -1
}

func (p *PulseMonitor) Start(volumeOut chan<- VolumeInfo) {
	volumeTrigger := make(chan struct{}, 1)
	p.client.Callback = func(val interface{}) {
		if ev, ok := val.(*pulse.SubscribeEvent); ok {
			if ev.Event.GetType() == pulse.EventChange && ev.Event.GetFacility() == pulse.EventSink {
				select {
				case volumeTrigger <- struct{}{}:
				default:
				}
			}
		}
	}

	if err := p.client.Request(&pulse.SetClientName{Props: pulse.PropList{}}, nil); err != nil {
		slog.Warn("Pulse SetClientName error", "error", err)
	}
	if err := p.client.Request(&pulse.Subscribe{Mask: pulse.SubscriptionMaskSink}, nil); err != nil {
		slog.Warn("Pulse Subscribe error", "error", err)
	}

	getSinkInfo := func() (pulse.GetSinkInfoReply, error) {
		repl := pulse.GetSinkInfoReply{}
		err := p.client.Request(&pulse.GetSinkInfo{SinkIndex: pulse.Undefined, SinkName: defaultSink}, &repl)
		return repl, err
	}

	volumeTrigger <- struct{}{}

	for {
		select {
		case <-volumeTrigger:
			repl, err := getSinkInfo()
			if err != nil {
				continue
			}
			if len(repl.ChannelVolumes) == 0 {
				continue
			}
			var acc int64
			for _, vol := range repl.ChannelVolumes {
				acc += int64(vol)
			}
			acc /= int64(len(repl.ChannelVolumes))
			volPct := int(float64(acc) / float64(pulse.VolumeNorm) * 100.0)
			volumeOut <- VolumeInfo{
				Volume: volPct,
				Mute:   repl.Mute,
			}

		case req := <-p.setVolumeChan:
			repl, err := getSinkInfo()
			if err != nil {
				continue
			}
			switch req {
			case -1: // Toggle mute
				p.client.Request(&pulse.SetSinkMute{SinkIndex: pulse.Undefined, SinkName: defaultSink, Mute: !repl.Mute}, nil)
			case -2: // Mute
				p.client.Request(&pulse.SetSinkMute{SinkIndex: pulse.Undefined, SinkName: defaultSink, Mute: true}, nil)
			case -3: // Unmute
				p.client.Request(&pulse.SetSinkMute{SinkIndex: pulse.Undefined, SinkName: defaultSink, Mute: false}, nil)
			default: // Volume percentage
				rawVol := uint32(float64(req) * float64(pulse.VolumeNorm) / 100.0)
				vols := make(pulse.ChannelVolumes, len(repl.ChannelVolumes))
				for i := range vols {
					vols[i] = rawVol
				}
				p.client.Request(&pulse.SetSinkMute{SinkIndex: pulse.Undefined, SinkName: defaultSink, Mute: false}, nil)
				p.client.Request(&pulse.SetSinkVolume{SinkIndex: pulse.Undefined, SinkName: defaultSink, ChannelVolumes: vols}, nil)
			}
		}
	}
}
