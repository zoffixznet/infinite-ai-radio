package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"iar/internal/trackbuffer"
)

func bufferCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "buffer",
		Short: "Manage the on-disk track buffer",
		Long: `Phased generation plans and renders songs ahead into an on-disk
buffer, which survives restarts. These commands inspect and reset it.`,
	}
	cmd.AddCommand(bufferClearCommand())
	return cmd
}

func bufferClearCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Delete every planned and rendered song, starting generation over",
		Long: `Removes all stored plans and rendered songs from the buffer. The next
cycle starts generation from scratch for the current session - the same
effect a fresh prompt has on the pipeline, without changing what plays.
A running player notices within seconds and begins refilling.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			buf := trackbuffer.New(filepath.Join(a.paths.DataDir, "buffer"), a.cfg.MP3Quality, a.log)
			if !yes {
				fmt.Print("Delete every buffered plan and song? [y/N] ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					fmt.Println("nothing deleted")
					return nil
				}
			}
			dropped := buf.DropAll()
			fmt.Printf("buffer cleared: %d file(s) removed\n", dropped)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "clear without asking")
	return cmd
}
