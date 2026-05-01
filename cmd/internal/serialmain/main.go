package serialmain

import (
	"fmt"
	"os"

	"go-serial-cli/internal/cli"
	"go-serial-cli/internal/skill"
)

func Main(commandName string) {
	deps, err := cli.DefaultDeps()
	if err != nil {
		fmt.Fprintln(os.Stderr, commandName+":", err)
		os.Exit(1)
	}
	deps.CommandName = commandName
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
		fmt.Fprintln(os.Stderr, commandName+":", err)
		os.Exit(1)
	}
}
