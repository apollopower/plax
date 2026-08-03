package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/alecthomas/kong"
	"github.com/apollopower/plax/pkg/blueprint"
)

type CLI struct {
	Init InitCmd `cmd:"" help:"Scaffold a blueprint by parsing the repo's docker-compose.yml and .env.example"`
}

type InitCmd struct {
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
}

func main() {
	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("plax"),
		kong.Description("Run many parallel dev environments for coding agents."),
		kong.UsageOnError(),
	)
	switch {
	case ctx.Command() == "init":
		err := runInit(cli.Init)
		ctx.FatalIfErrorf(err)
	}
}

func runInit(cmd InitCmd) error {
	bp, err := blueprint.InitFromRepo(cmd.Root)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(bp); err != nil {
		return fmt.Errorf("init: encoding output: %w", err)
	}

	return nil
}
