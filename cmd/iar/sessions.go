package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"iar/internal/session"
)

// sessionsCommand lists sessions and presets, and groups delete/restore.
func sessionsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List your sessions, the presets and auto-saved sessions",
		Long: `Lists everything you can start: your named sessions (most recently
played first), the built-in presets, and sessions that were saved
automatically under a generated name (newest first). Auto-saved
sessions are removed after they have not played for a while
(sessions.auto_retention_days in the config, default 2; 0 keeps them).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			store := session.NewStore(a.paths.SessionsDir())
			l, err := store.Listing()
			if err != nil {
				return err
			}
			fmt.Println(l.Render(time.Now()))
			fmt.Println()
			fmt.Println("start one:  iar --session NAME   or   iar --preset NAME")
			fmt.Println("delete one: iar sessions delete NAME")
			if d := a.cfg.Sessions.AutoRetentionDays; d > 0 {
				fmt.Printf("auto-saved sessions are removed %d day(s) after they last played; name one to keep it\n", d)
			}
			return nil
		},
	}
	cmd.AddCommand(sessionsDeleteCommand(), sessionsRestoreCommand())
	return cmd
}

func sessionsDeleteCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a saved session, or hide a built-in preset",
		Long: `Deletes a saved session after asking for confirmation. A preset name
hides that preset from every listing instead (presets are built in);
'iar sessions restore-presets' brings hidden presets back. The session
currently playing cannot be deleted.`,
		Example: `  iar sessions delete gym-grind
  iar sessions delete sleep --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			name := session.SanitizeName(args[0])
			store := session.NewStore(a.paths.SessionsDir())
			if cs, playing := a.stateD.ReadCurrentSession(); playing && cs.Name == name {
				return fmt.Errorf("%s is playing right now; switch sessions or quit the player first", name)
			}
			isPreset := false
			if _, err := store.LookupPreset(name); err == nil {
				isPreset = true
			} else if _, err := store.Load(name); err != nil {
				if errors.Is(err, session.ErrNotFound) {
					return fmt.Errorf("no session or preset named %s (see 'iar sessions')", name)
				}
				return err
			}
			if !yes {
				what := "session"
				if isPreset {
					what = "preset"
				}
				fmt.Printf("Delete %s %s? [y/N] ", what, name)
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				switch strings.ToLower(strings.TrimSpace(line)) {
				case "y", "yes":
				default:
					fmt.Println("cancelled")
					return nil
				}
			}
			if isPreset {
				if err := store.HidePreset(name); err != nil {
					return err
				}
				fmt.Printf("preset %s hidden (restore every preset with: iar sessions restore-presets)\n", name)
				return nil
			}
			if err := store.Delete(name); err != nil {
				return err
			}
			a.stateD.ForgetSession(name)
			a.log.Info("session deleted", "event", "session_deleted", "session", name)
			fmt.Printf("session %s deleted\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "delete without asking")
	return cmd
}

func sessionsRestoreCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restore-presets",
		Short: "Bring back every deleted built-in preset",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			n, err := session.NewStore(a.paths.SessionsDir()).RestorePresets()
			if err != nil {
				return err
			}
			if n == 0 {
				fmt.Println("no deleted presets; nothing to restore")
				return nil
			}
			fmt.Printf("restored %d preset(s)\n", n)
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
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			store := session.NewStore(a.paths.SessionsDir())
			fmt.Println("built-in presets:")
			for _, g := range session.GroupPresets(store.Presets()) {
				fmt.Printf(" %s:\n", g.Name)
				for _, p := range g.Presets {
					fmt.Printf("  %-18s %s\n", p.Name, p.Description)
				}
			}
			if hidden := store.HiddenPresets(); len(hidden) > 0 {
				fmt.Printf("  (%d deleted; bring back with: iar sessions restore-presets)\n", len(hidden))
			}
			fmt.Println("\nstart one with: iar --preset NAME")
			fmt.Println("switch inside the player with: preset NAME")
			fmt.Println(`or skip presets and start from a prompt: iar "dark techno"`)
			return nil
		},
	}
}
