package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"bgm/internal/session"
)

// sessionsCommand lists built-in presets and saved sessions.
func sessionsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "List saved sessions (and the built-in presets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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
		},
	}
}

// presetsCommand lists the built-in presets.
func presetsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "presets",
		Short: "List the built-in presets",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("built-in presets:")
			for _, p := range session.Presets() {
				fmt.Printf("  %-12s %s\n", p.Name, p.Description)
			}
			fmt.Println("\nstart one with: bgm --preset NAME")
			fmt.Println("switch inside the player with: preset NAME")
		},
	}
}
