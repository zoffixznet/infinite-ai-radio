package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"iar/internal/state"
	"iar/internal/trackbuffer"
)

func bufferCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "buffer",
		Short: "Manage the on-disk song store",
		Long: `The radio plans and renders songs ahead into an on-disk store, which
survives restarts. These commands reset it, or keep it across a build.`,
	}
	cmd.AddCommand(bufferClearCommand())
	cmd.AddCommand(bufferKeepCommand())
	return cmd
}

func bufferClearCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Delete every planned and rendered song, starting generation over",
		Long: `Removes all stored plans and rendered songs from the store. The next
start begins generation from scratch for the current session - the same
effect a fresh prompt has, without changing what plays. A running radio
owns its store, so this refuses while one is running: use its 'restart'
command, or the phone's "Empty the buffer and start over", instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			// The running radio keeps the store's listing in memory and
			// is the only thing that may change what is in it.
			lock, err := a.stateD.AcquireLock("player.lock", false)
			if err != nil {
				if err == state.ErrLocked {
					return fmt.Errorf("the radio is running and owns its store; type 'restart' in it, or use the phone's \"Empty the buffer and start over\"")
				}
				return err
			}
			defer lock.Release()
			buf := trackbuffer.New(filepath.Join(a.paths.DataDir, "buffer"), a.cfg.MP3Quality, a.log)
			if !yes {
				fmt.Print("Delete every stored plan and song? [y/N] ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					fmt.Println("nothing deleted")
					return nil
				}
			}
			dropped := buf.DropAll()
			fmt.Printf("store cleared: %d file(s) removed\n", dropped)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "clear without asking")
	return cmd
}

func bufferKeepCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "keep",
		Short: "Stamp the store as made by this build, so the next start keeps its songs",
		Long: `A new build of the radio clears the store on its first start, because
newer code may make songs differently and yesterday's songs should not
speak for it. When a build changes nothing about the songs - a fix to
the phone, say - run this before starting it: the store is stamped as
made by this build and the next start adopts it, songs and all. The
radio must be stopped; a running one owns its store.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			buf := trackbuffer.New(filepath.Join(a.paths.DataDir, "buffer"), a.cfg.MP3Quality, a.log)
			was, songs, err := keepStore(a.stateD, buf, version)
			if err != nil {
				return err
			}
			switch {
			case was == version:
				fmt.Printf("store already made by this build (%s): %d song(s) kept\n", version, songs)
			case was == "":
				fmt.Printf("store stamped %s: %d song(s) kept\n", version, songs)
			default:
				fmt.Printf("store stamped %s (was %s): %d song(s) kept\n", version, was, songs)
			}
			return nil
		},
	}
}

// keepStore stamps the store with build so the next start adopts it
// instead of clearing it. It reports the stamp the store had and how
// many songs it holds, and refuses while a radio owns the store.
func keepStore(stateD state.Dir, buf *trackbuffer.Store, build string) (was string, songs int, err error) {
	lock, err := stateD.AcquireLock("player.lock", false)
	if err != nil {
		if err == state.ErrLocked {
			return "", 0, fmt.Errorf("the radio is running and owns its store; quit it first, then run this before starting the new build")
		}
		return "", 0, err
	}
	defer lock.Release()
	was = buf.Build()
	songs, _ = buf.TrackStats(0)
	buf.SetBuild(build)
	return was, songs, nil
}
