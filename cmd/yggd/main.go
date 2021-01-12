package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"git.sr.ht/~spc/go-log"
	"github.com/redhatinsights/yggdrasil"
	internal "github.com/redhatinsights/yggdrasil/internal"
	"github.com/urfave/cli/v2"
	"github.com/urfave/cli/v2/altsrc"
)

func main() {
	app := cli.NewApp()
	app.Version = yggdrasil.Version

	defaultConfigFilePath, err := yggdrasil.ConfigPath()
	if err != nil {
		log.Fatal(err)
	}

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:      "config",
			Value:     defaultConfigFilePath,
			TakesFile: true,
		},
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:  "log-level",
			Value: "info",
		}),
		altsrc.NewStringSliceFlag(&cli.StringSliceFlag{
			Name: "broker",
		}),
		&cli.BoolFlag{
			Name:   "generate-man-page",
			Hidden: true,
		},
		&cli.BoolFlag{
			Name:   "generate-markdown",
			Hidden: true,
		},
	}

	// This BeforeFunc will load flag values from a config file only if the
	// "config" flag value is non-zero.
	app.Before = func(c *cli.Context) error {
		filePath := c.String("config")
		if filePath != "" {
			inputSource, err := altsrc.NewTomlSourceFromFile(filePath)
			if err != nil {
				return err
			}
			return altsrc.ApplyInputSourceValues(c, inputSource, app.Flags)
		}
		return nil
	}

	app.Action = func(c *cli.Context) error {
		if c.Bool("generate-man-page") || c.Bool("generate-markdown") {
			type GenerationFunc func() (string, error)
			var generationFunc GenerationFunc
			if c.Bool("generate-man-page") {
				generationFunc = c.App.ToMan
			} else if c.Bool("generate-markdown") {
				generationFunc = c.App.ToMarkdown
			}
			data, err := generationFunc()
			if err != nil {
				return err
			}
			fmt.Println(data)
			return nil
		}
		level, err := log.ParseLevel(c.String("log-level"))
		if err != nil {
			return cli.NewExitError(err, 1)
		}
		log.SetLevel(level)
		log.SetPrefix(fmt.Sprintf("[%v] ", app.Name))

		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL)

		processManager, err := yggdrasil.NewProcessManager()
		if err != nil {
			return cli.NewExitError(err, 1)
		}

		dispatcher, err := yggdrasil.NewDispatcher()
		if err != nil {
			return cli.NewExitError(err, 1)
		}

		messageRouter, err := yggdrasil.NewMessageRouter(c.StringSlice("broker"))
		if err != nil {
			return cli.NewExitError(err, 1)
		}

		payloadProcessor, err := yggdrasil.NewPayloadProcessor()
		if err != nil {
			return cli.NewExitError(err, 1)
		}

		// Connect dispatcher to the processManager's "process-die" signal
		sigProcessDie := processManager.Connect(yggdrasil.SignalProcessDie)
		go dispatcher.HandleProcessDieSignal(sigProcessDie)

		// Connect payloadProcessor to the messageRouter's "message-recv" signal
		sigMessageRecv := messageRouter.Connect(yggdrasil.SignalMessageRecv)
		go payloadProcessor.HandleMessageRecvSignal(sigMessageRecv)

		// Connect dispatcher to the payloadProcessor's "assignment-create" signal
		sigAssignmentCreate := payloadProcessor.Connect(yggdrasil.SignalAssignmentCreate)
		go dispatcher.HandleAssignmentCreateSignal(sigAssignmentCreate)

		// Connect payloadProcessor to the dispatcher's "work-complete" signal
		sigWorkComplete := dispatcher.Connect(yggdrasil.SignalWorkComplete)
		go payloadProcessor.HandleWorkCompleteSignal(sigWorkComplete)

		// Connect dispatcher to the payloadProcessor's "assignment-return" signal
		sigAssignmentReturn := payloadProcessor.Connect(yggdrasil.SignalAssignmentReturn)
		go dispatcher.HandleAssignmentReturnSignal(sigAssignmentReturn)

		// ProcessManager goroutine
		sigDispatcherListen := dispatcher.Connect(yggdrasil.SignalDispatcherListen)
		go func(c <-chan interface{}) {
			logger := log.New(os.Stderr, fmt.Sprintf("%v[process_manager_routine] ", log.Prefix()), log.Flags(), log.CurrentLevel())
			logger.Trace("init")

			<-c

			p := filepath.Join(yggdrasil.LibexecDir, yggdrasil.LongName)
			os.MkdirAll(p, 0755)
			if localErr := processManager.BootstrapWorkers(p); localErr != nil {
				err = localErr
				quit <- syscall.SIGTERM
			}
		}(sigDispatcherListen)

		// Dispatcher goroutine
		go func() {
			logger := log.New(os.Stderr, fmt.Sprintf("%v[dispatcher_routine] ", log.Prefix()), log.Flags(), log.CurrentLevel())
			logger.Trace("init")

			if localErr := dispatcher.ListenAndServe(); localErr != nil {
				logger.Trace(localErr)
				err = localErr
				quit <- syscall.SIGTERM
			}
		}()

		// MessageRouter goroutine
		sigProcessBootstrap := processManager.Connect(yggdrasil.SignalProcessBootstrap)
		go func(c <-chan interface{}) {
			logger := log.New(os.Stderr, fmt.Sprintf("%v[message_router_routine] ", log.Prefix()), log.Flags(), log.CurrentLevel())
			logger.Trace("init")

			<-c

			if localError := messageRouter.ConnectClient(); localError != nil {
				err = localError
				quit <- syscall.SIGTERM
			}

			facts, localErr := yggdrasil.GetCanonicalFacts()
			if localErr != nil {
				err = localErr
				quit <- syscall.SIGTERM
			}
			data, localErr := json.Marshal(facts)
			if localErr != nil {
				err = localErr
				quit <- syscall.SIGTERM
			}
			if localErr := messageRouter.Publish("handshake", data); localErr != nil {
				err = localErr
				quit <- syscall.SIGTERM
			}

			if localErr := messageRouter.Subscribe(); localErr != nil {
				err = localErr
				quit <- syscall.SIGTERM
			}
		}(sigProcessBootstrap)

		<-quit

		if err := processManager.KillAllWorkers(); err != nil {
			return cli.NewExitError(err, 1)
		}

		if err != nil {
			return cli.NewExitError(err, 1)
		}

		return nil
	}
	app.EnableBashCompletion = true
	app.BashComplete = internal.BashComplete

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
