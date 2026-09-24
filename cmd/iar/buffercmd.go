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
survives restarts. These commands reset it.`,
	}
	cmd.AddCommand(bufferClearCommand())
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
