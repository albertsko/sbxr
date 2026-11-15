package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/albertsko/sbxr/internal/image"
	"github.com/albertsko/sbxr/internal/spec"
)

const pruneName = "prune"

func (a *app) pruneCommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   pruneName,
		Short: "Remove cached images",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := a.prune(cmd, force)
			if exit, ok := errors.AsType[*exitError](err); ok {
				return exit
			}

			if err != nil {
				return failure(cmd, err)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Do not ask for confirmation")

	return cmd
}

func (a *app) prune(cmd *cobra.Command, force bool) error {
	dir, err := cacheDir()
	if err != nil {
		return err
	}

	if !force {
		ok, err := confirm(cmd, "Remove all cached images in "+dir+"? [y/N] ")
		if err != nil || !ok {
			return err
		}
	}

	if a.backend.Isolate != nil {
		code, err := a.backend.Isolate([]string{pruneName, "--force"}, os.Stdin, a.stdout.real, os.Stderr)
		if err == nil && code != 0 {
			err = &exitError{code: code, err: nil}
		}

		return err
	}

	store, err := a.backend.Images(dir)
	if err != nil {
		return err
	}

	err = store.Cache.Prune()
	if errors.Is(err, image.ErrBusy) {
		return &spec.Error{Source: pruneName, Problem: "a running sbxr run uses the image cache", Fix: "try again after it ends"}
	}

	return err //nolint:wrapcheck // Prune errors name the cache.
}

// confirm asks a yes or no question on the terminal. It fails when stdin is not a terminal.
func confirm(cmd *cobra.Command, question string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, &spec.Error{Source: pruneName, Problem: "cannot ask for confirmation, because stdin is not a terminal", Fix: "pass --force"}
	}

	_, err := io.WriteString(cmd.ErrOrStderr(), question)
	if err != nil {
		return false, err //nolint:wrapcheck // A failed prompt write needs no context.
	}

	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read answer: %w", err)
	}

	answer = strings.ToLower(strings.TrimSpace(answer))

	return answer == "y" || answer == "yes", nil
}
