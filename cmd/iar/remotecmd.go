package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"iar/internal/accounts"
	"iar/internal/mail"
)

// remoteCommand groups the phone remote's account commands.
func remoteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Set up accounts for the phone remote",
		Long: `The phone remote (started with --remote) requires a login. These
commands create the first admin account and check the optional email
setup; everything else (inviting listeners, permissions, password
resets) happens on the remote's Users page.`,
	}
	cmd.AddCommand(remoteSetupCommand(), remoteTestEmailCommand())
	return cmd
}

func remoteSetupCommand() *cobra.Command {
	var email string
	var passwordStdin bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Create the first admin account (or reset an admin's password)",
		Long: `Creates the first admin account for the phone remote with every
permission. Run interactively it asks for the email and the password
(typed twice, hidden). For scripts, pass --email and --password-stdin.

Running it again for an existing email resets that account's password
and restores every permission, which is how a locked-out admin gets
back in.`,
		Example: `  iar remote setup
  echo "$PASSWORD" | iar remote setup --email you@example.com --password-stdin`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			users, _ := a.accountStores()

			in := bufio.NewReader(os.Stdin)
			if email == "" {
				if !isTerminal(os.Stdin) {
					return errors.New("no terminal: pass --email (and --password-stdin)")
				}
				fmt.Print("Admin email: ")
				line, _ := in.ReadString('\n')
				email = strings.TrimSpace(line)
			}
			if _, err := accounts.NormalizeEmail(email); err != nil {
				return err
			}
			var password string
			switch {
			case passwordStdin:
				line, err := in.ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				password = strings.TrimRight(line, "\r\n")
			case isTerminal(os.Stdin):
				password, err = askPasswordTwice()
				if err != nil {
					return err
				}
			default:
				return errors.New("no terminal to ask for the password: pass --password-stdin")
			}
			created, err := users.EnsureAdmin(email, password)
			if err != nil {
				return err
			}
			norm, _ := accounts.NormalizeEmail(email)
			if created {
				fmt.Printf("Admin account %s created with every permission.\n", norm)
			} else {
				fmt.Printf("Account %s already existed: password reset and every permission granted.\n", norm)
			}
			fmt.Printf("Accounts live in %s. Start the player with --remote and log in on the page.\n", users.Path())
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "admin email address (asked interactively when omitted)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from the first line of standard input")
	return cmd
}

// askPasswordTwice reads a password twice without echo and insists the
// two match and meet the policy.
func askPasswordTwice() (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Printf("Password (%d+ characters): ", accounts.MinPasswordLength)
		p1, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		if len(p1) < accounts.MinPasswordLength {
			fmt.Println(accounts.ErrWeakPassword.Error())
			continue
		}
		fmt.Print("Same password again: ")
		p2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		if string(p1) != string(p2) {
			fmt.Println("The two passwords do not match; try again.")
			continue
		}
		return string(p1), nil
	}
	return "", errors.New("giving up after three attempts")
}

func remoteTestEmailCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "test-email <address>",
		Short: "Send a test message through the configured SMTP server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			to, err := accounts.NormalizeEmail(args[0])
			if err != nil {
				return err
			}
			cfg := a.cfg.Remote.SMTP
			if !cfg.Configured() {
				return errors.New("no SMTP server configured; email is optional, but to use it set remote.smtp in " + a.paths.ConfigFile())
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
			defer cancel()
			fmt.Printf("Sending a test message to %s via %s...\n", to, cfg.Host)
			body := "This is a test message from Infinite AI Radio.\n\nIf you can read this, invite and reset links will be emailed automatically.\n"
			if err := mail.New(cfg).Send(ctx, to, "Infinite AI Radio test message", body); err != nil {
				return err
			}
			fmt.Println("Sent. Check the inbox (and the spam folder).")
			return nil
		},
	}
}
