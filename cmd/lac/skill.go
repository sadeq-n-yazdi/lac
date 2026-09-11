package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"sadeq.uk/lac/internal/skill"
)

// runSkill installs the bundled skill where an AI tool will find it, so an agent learns the
// etiquette — queue before heavy work, say what you are doing — without the operator writing
// anything themselves.
func runSkill(_ context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("skill", flag.ContinueOnError)
	var (
		directory = flags.String("dir", "", "where to install (default: ~/.claude/skills)")
		toStdout  = flags.Bool("print", false, "write the skill to stdout instead of installing it")
	)

	if err := parseAnywhere(flags, arguments); err != nil {
		return err
	}

	action := flags.Arg(0)
	if action == "" {
		action = "install"
	}

	switch action {
	case "install":
		if *toStdout {
			return printSkill()
		}
		return installSkill(env, *directory)

	case "print":
		return printSkill()

	case "path":
		target, err := skill.DefaultDirectory()
		if err != nil {
			return err
		}
		fmt.Fprintf(env.output, "%s/%s/SKILL.md\n", target, skill.Name)

		return nil

	default:
		return errors.New("usage: lac skill [install|print|path]")
	}
}

func printSkill() error {
	if _, err := fmt.Fprint(os.Stdout, skill.Content()); err != nil {
		return fmt.Errorf("writing the skill: %w", err)
	}

	return nil
}

func installSkill(env *environment, directory string) error {
	result, err := skill.Install(directory)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, map[string]any{
			"path": result.Path, "updated": result.Updated, "unchanged": result.Unchanged,
		})
	}

	switch {
	case result.Unchanged:
		fmt.Fprintf(env.output, "already installed\t%s\n", result.Path)
	case result.Updated:
		fmt.Fprintf(env.output, "updated\t%s\n", result.Path)
	default:
		fmt.Fprintf(env.output, "installed\t%s\n", result.Path)
	}

	return nil
}
