// Package mpris exposes the running player on the session D-Bus as an
// MPRIS media player (org.mpris.MediaPlayer2.bgm), so desktop media keys,
// playerctl, KDE Connect and similar tooling can pause, skip and set the
// volume.
package mpris

import (
	"context"
	"log/slog"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// BusName is the well-known MPRIS name bgm claims.
const BusName = "org.mpris.MediaPlayer2.bgm"

const objectPath = "/org/mpris/MediaPlayer2"

// Controls is what the MPRIS surface needs from the player. The
// orchestrator implements it.
type Controls interface {
	Pause() string
	Resume() string
	TogglePause() string
	Skip() string
	SetVolume(v int) string
	// Snapshot returns paused state, volume percent and a display title.
	Snapshot() (paused bool, volume int, title string)
}

// Server is a running MPRIS endpoint.
type Server struct {
	conn  *dbus.Conn
	props *prop.Properties
	ctl   Controls
	log   *slog.Logger
}

// Start connects to the session bus and exports the player. It returns an
// error when no session bus is available; callers treat that as optional.
func Start(ctx context.Context, ctl Controls, log *slog.Logger) (*Server, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	s := &Server{conn: conn, ctl: ctl, log: log}

	root := &mprisRoot{}
	player := &mprisPlayer{s: s}
	if err := conn.Export(root, objectPath, "org.mpris.MediaPlayer2"); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Export(player, objectPath, "org.mpris.MediaPlayer2.Player"); err != nil {
		conn.Close()
		return nil, err
	}

	paused, volume, title := ctl.Snapshot()
	propsSpec := prop.Map{
		"org.mpris.MediaPlayer2": {
			"Identity":            {Value: "bgm", Emit: prop.EmitTrue},
			"CanQuit":             {Value: false, Emit: prop.EmitTrue},
			"CanRaise":            {Value: false, Emit: prop.EmitTrue},
			"HasTrackList":        {Value: false, Emit: prop.EmitTrue},
			"SupportedUriSchemes": {Value: []string{}, Emit: prop.EmitTrue},
			"SupportedMimeTypes":  {Value: []string{}, Emit: prop.EmitTrue},
		},
		"org.mpris.MediaPlayer2.Player": {
			"PlaybackStatus": {Value: statusString(paused), Emit: prop.EmitTrue},
			"Volume": {Value: float64(volume) / 100, Writable: true, Emit: prop.EmitTrue,
				Callback: s.onVolume},
			"Metadata":      {Value: metadata(title), Emit: prop.EmitTrue},
			"CanGoNext":     {Value: true, Emit: prop.EmitTrue},
			"CanGoPrevious": {Value: false, Emit: prop.EmitTrue},
			"CanPlay":       {Value: true, Emit: prop.EmitTrue},
			"CanPause":      {Value: true, Emit: prop.EmitTrue},
			"CanSeek":       {Value: false, Emit: prop.EmitTrue},
			"CanControl":    {Value: true, Emit: prop.EmitTrue},
			"MinimumRate":   {Value: 1.0, Emit: prop.EmitTrue},
			"MaximumRate":   {Value: 1.0, Emit: prop.EmitTrue},
			"Rate":          {Value: 1.0, Emit: prop.EmitTrue},
			"LoopStatus":    {Value: "None", Emit: prop.EmitTrue},
			"Shuffle":       {Value: false, Emit: prop.EmitTrue},
			"Position":      {Value: int64(0), Emit: prop.EmitFalse},
		},
	}
	s.props, err = prop.Export(conn, objectPath, propsSpec)
	if err != nil {
		conn.Close()
		return nil, err
	}

	node := &introspect.Node{
		Name: objectPath,
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name:    "org.mpris.MediaPlayer2",
				Methods: introspect.Methods(root),
			},
			{
				Name:    "org.mpris.MediaPlayer2.Player",
				Methods: introspect.Methods(player),
			},
		},
	}
	if err := conn.Export(introspect.NewIntrospectable(node), objectPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		conn.Close()
		return nil, err
	}

	reply, err := conn.RequestName(BusName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		if err == nil {
			err = dbus.ErrMsgNoObject
		}
		return nil, err
	}

	go s.refreshLoop(ctx)
	log.Info("mpris endpoint up", "event", "mpris_up", "bus_name", BusName)
	return s, nil
}

// Close releases the bus name and connection.
func (s *Server) Close() {
	if s == nil || s.conn == nil {
		return
	}
	s.conn.ReleaseName(BusName)
	s.conn.Close()
}

// onVolume handles external Volume writes.
func (s *Server) onVolume(c *prop.Change) *dbus.Error {
	v, ok := c.Value.(float64)
	if !ok {
		return prop.ErrInvalidArg
	}
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	s.ctl.SetVolume(int(v*100 + 0.5))
	s.log.Info("volume set via mpris", "event", "mpris_volume", "volume", v)
	return nil
}

// refreshLoop mirrors player state into the D-Bus properties.
func (s *Server) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Close()
			return
		case <-ticker.C:
		}
		paused, volume, title := s.ctl.Snapshot()
		s.props.SetMust("org.mpris.MediaPlayer2.Player", "PlaybackStatus", statusString(paused))
		s.props.SetMust("org.mpris.MediaPlayer2.Player", "Metadata", metadata(title))
		// Only push volume when it changed elsewhere, to avoid fighting
		// an in-flight external write.
		cur, err := s.props.Get("org.mpris.MediaPlayer2.Player", "Volume")
		if err == nil {
			if f, ok := cur.Value().(float64); ok && int(f*100+0.5) != volume {
				s.props.SetMust("org.mpris.MediaPlayer2.Player", "Volume", float64(volume)/100)
			}
		}
	}
}

func statusString(paused bool) string {
	if paused {
		return "Paused"
	}
	return "Playing"
}

func metadata(title string) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(dbus.ObjectPath("/org/bgm/track/current")),
		"xesam:title":   dbus.MakeVariant(title),
		"xesam:artist":  dbus.MakeVariant([]string{"bgm"}),
	}
}

// mprisRoot implements org.mpris.MediaPlayer2.
type mprisRoot struct{}

// Raise implements the MPRIS root interface (no window to raise).
func (r *mprisRoot) Raise() *dbus.Error { return nil }

// Quit implements the MPRIS root interface (quitting is interactive).
func (r *mprisRoot) Quit() *dbus.Error { return nil }

// mprisPlayer implements org.mpris.MediaPlayer2.Player methods.
type mprisPlayer struct {
	s *Server
}

// Next skips to the next track.
func (p *mprisPlayer) Next() *dbus.Error {
	p.s.ctl.Skip()
	return nil
}

// Previous is unsupported (tracks are generated, not a playlist).
func (p *mprisPlayer) Previous() *dbus.Error { return nil }

// Pause pauses output.
func (p *mprisPlayer) Pause() *dbus.Error {
	p.s.ctl.Pause()
	return nil
}

// PlayPause toggles pause.
func (p *mprisPlayer) PlayPause() *dbus.Error {
	p.s.ctl.TogglePause()
	return nil
}

// Play resumes output.
func (p *mprisPlayer) Play() *dbus.Error {
	p.s.ctl.Resume()
	return nil
}

// Stop pauses output (the stream has no stopped state).
func (p *mprisPlayer) Stop() *dbus.Error {
	p.s.ctl.Pause()
	return nil
}

// Seek, SetPosition and OpenUri are deliberately not exported: CanSeek is
// false and the stream is generated, not seekable media.
