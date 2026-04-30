package main

import (
	"fmt"
	"os"

	"go-serial-cli/internal/cli"
	"go-serial-cli/internal/skill"
)

func main() {
	deps, err := cli.DefaultDeps()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gs:", err)
		os.Exit(1)
	}
	deps.InstallSkill = func(source string, to string) error {
		result, err := skill.Install(skill.InstallOptions{
			Source: source,
			To:     to,
		})
		if err != nil {
			return err
		}
		for _, path := range result.Installed {
			fmt.Fprintln(os.Stdout, "installed", path)
		}
		return nil
	}

	app := cli.New(deps)

	if err := app.Run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gs:", err)
		os.Exit(1)
	}
}
