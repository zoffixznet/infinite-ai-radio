package main

import (
	"flag"
	"fmt"

	"bgm/internal/session"
)

// cmdSessions lists built-in presets and saved sessions.
func cmdSessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	fs.Parse(args)

	a, err := newApp(false)
	if err != nil {
		return err
	}
	defer a.close()

	fmt.Println("presets (bgm --preset NAME):")
	for _, p := range session.Presets() {
		fmt.Printf("  %-12s %s\n", p.Name, p.Description)
	}
	store := session.NewStore(a.paths.SessionsDir())
	saved, err := store.List()
	if err != nil {
		return err
	}
	if len(saved) == 0 {
		fmt.Println("no saved sessions yet (in the player, type: name <something>)")
		return nil
	}
	fmt.Println("saved sessions (bgm --session NAME):")
	for _, s := range saved {
		fmt.Printf("  %-24s %s\n", s.Name, s.Describe())
	}
	return nil
}
