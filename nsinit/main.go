package main

import (
	"os"

	log "github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
	"github.com/docker/libcontainer"
)

func main() {
	app := cli.NewApp()
	app.Name = "nsinit"
	app.Version = "2"
	app.Author = "libcontainer maintainers"
	app.Flags = []cli.Flag{
		cli.BoolFlag{Name: "debug", Usage: "enable debug output in the logs"},
		cli.StringFlag{Name: "root", Value: "/var/run/nsinit", Usage: "root directory for containers"},
		cli.StringFlag{Name: "log-file", Usage: "set the log file to output logs to"},
		cli.StringFlag{Name: "criu", Value: "criu", Usage: "path to the criu binary for checkpoint and restore"},
	}
	app.Commands = []cli.Command{
		checkpointCommand,
		configCommand,
		execCommand,
		initCommand,
		oomCommand,
		pauseCommand,
		stateCommand,
		statsCommand,
		unpauseCommand,
		restoreCommand,
	}
	app.Before = func(context *cli.Context) error {
		if context.GlobalBool("debug") {
			log.SetLevel(log.DebugLevel)
		}
		if path := context.GlobalString("log-file"); path != "" {
			f, err := os.Create(path)
			if err != nil {
				return err
			}
			log.SetOutput(f)
		}
		if criuPath := context.GlobalString("criu"); criuPath != "" {
			libcontainer.CriuPath = criuPath
		}
		return nil
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
