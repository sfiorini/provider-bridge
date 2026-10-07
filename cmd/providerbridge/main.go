package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"log/slog"
	"providerbridge/internal/config"
	"providerbridge/internal/extension/codex"
	"providerbridge/internal/logger"
	"providerbridge/internal/service/app"
)

const (
	exitOK          = 0
	exitRuntimeErr  = 1
	exitStartupErr  = 2
	defaultProgName = "providerbridge"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet(defaultProgName, flag.ContinueOnError)
	flags.SetOutput(stderr)

	configPath := flags.String("config", "", "Path to config.yml")
	addr := flags.String("addr", "", "Override server listen address")
	mode := flags.String("mode", "", "Override mode: CaptureAnthropic, CaptureResponse, or Transform")
	printAddr := flags.Bool("print-addr", false, "Print configured listen address and exit")
	printMode := flags.Bool("print-mode", false, "Print configured mode and exit")
	printDefaultModel := flags.Bool("print-default-model", false, "Print configured default model alias and exit")
	printCodexModel := flags.Bool("print-codex-model", false, "Print configured Codex model and exit")
	printClaudeModel := flags.Bool("print-claude-model", false, "Print configured Claude Code model and exit")
	printCodexConfig := flags.String("print-codex-config", "", "Print Codex config.toml for the model alias and exit")
	dumpConfigSchema := flags.Bool("dump-config-schema", false, "Generate config.schema.json alongside config and exit")
	codexBaseURL := flags.String("codex-base-url", "", "Base URL to write in generated Codex config")
	codexHome := flags.String("codex-home", "", "CODEX_HOME directory; when set, writes models_catalog.json there")
	if err := flags.Parse(args); err != nil {
		return exitStartupErr
	}
	configFlagSet := flagWasSet(flags, "config")

	var cfg config.Config
	var err error
	extensions := app.BuiltinExtensions()
	resolvedConfigPath, err := config.ResolveConfigPath(*configPath)
	if err != nil {
		writeStartupError(stderr, "failed to resolve config file path", "", err,
			"Set HOME, or use -config to specify the config file path explicitly.")
		return exitStartupErr
	}
	if *dumpConfigSchema {
		if err := app.DumpConfigSchema(resolvedConfigPath); err != nil {
			writeStartupError(stderr, "schema dump failed", resolvedConfigPath, err)
			return exitStartupErr
		}
		fmt.Fprintln(stdout, resolvedConfigPath)
		return exitOK
	}

	if !configFlagSet {
		created, err := createStarterConfigIfMissing(resolvedConfigPath, config.LoadOptions{
			ExtensionSpecs: extensions.ConfigSpecs(),
		})
		if err != nil {
			writeStartupError(stderr, "failed to create default config", resolvedConfigPath, err,
				"Verify the HOME directory is writable, or point -config at an existing config file.")
			return exitStartupErr
		}
		if created {
			fmt.Fprintf(stderr, "created default config: %s\n", resolvedConfigPath)
		}
	}

	cfg, err = config.LoadFromFileWithOptions(resolvedConfigPath, config.LoadOptions{
		ExtensionSpecs: extensions.ConfigSpecs(),
	})
	if err != nil {
		writeStartupError(stderr, "failed to load config file", resolvedConfigPath, err,
			"When -config is omitted, the default is $HOME/provider-bridge/config.yml.",
			"Check YAML syntax, field spelling and indentation.",
			"Ensure required config such as provider, routes and developer.proxy is present.",
			"For the protocol field, use openai-response for Responses passthrough.")
		return exitStartupErr
	}
	if err := logger.Init(logger.Config{Level: logger.Level(cfg.LogLevel), Format: cfg.LogFormat, Output: stderr}); err != nil {
		writeStartupError(stderr, "failed to initialize logging", resolvedConfigPath, err,
			"Check that log.level and log.format use supported values.")
		return exitStartupErr
	}
	slog.Info("config loaded", "path", resolvedConfigPath, "mode", cfg.Mode, "addr", cfg.Addr)
	if *mode != "" {
		cfg.Mode = config.Mode(*mode)
		if err := cfg.Validate(); err != nil {
			writeStartupError(stderr, "config validation failed", resolvedConfigPath, fmt.Errorf("-mode %q: %w", *mode, err),
				"Check that -mode is Transform, CaptureResponse or CaptureAnthropic.",
				"The provider / developer.proxy config for that mode must also be complete.")
			return exitStartupErr
		}
	}
	if *addr != "" {
		cfg.OverrideAddr(*addr)
	}
	if *printAddr {
		fmt.Fprintln(stdout, cfg.Addr)
		return exitOK
	}
	if *printMode {
		fmt.Fprintln(stdout, cfg.Mode)
		return exitOK
	}
	if *printDefaultModel {
		fmt.Fprintln(stdout, cfg.DefaultModelAlias())
		return exitOK
	}
	if *printCodexModel {
		fmt.Fprintln(stdout, cfg.CodexModel())
		return exitOK
	}
	if *printClaudeModel {
		fmt.Fprintln(stdout, cfg.AnthropicProxy.Model)
		return exitOK
	}
	if *printCodexConfig != "" {
		if err := codex.GenerateConfigToml(stdout, *printCodexConfig, *codexBaseURL, *codexHome,
			config.ProviderFromGlobalConfig(&cfg), config.PluginFromGlobalConfig(&cfg), config.ServerFromGlobalConfig(&cfg)); err != nil {
			writeStartupError(stderr, "failed to generate Codex config", resolvedConfigPath, err,
				"Verify the -codex-home directory is writable, or drop -codex-home to only print config.toml.")
			return exitRuntimeErr
		}
		return exitOK
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	if err := app.RunServer(ctx, cfg, stderr); err != nil {
		writeStartupError(stderr, "server run failed", resolvedConfigPath, err,
			"Check whether the listen address is in use and that the upstream provider config is usable.")
		return exitRuntimeErr
	}
	return exitOK
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	wasSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func createStarterConfigIfMissing(configPath string, opts config.LoadOptions) (bool, error) {
	if _, err := os.Stat(configPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat config %s: %w", configPath, err)
	}

	dbPath, err := config.StarterSQLiteDBPath(configPath)
	if err != nil {
		return false, err
	}
	data, err := config.StarterConfigYAML(dbPath, opts)
	if err != nil {
		return false, err
	}
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return false, fmt.Errorf("create config directory %s: %w", configDir, err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return false, fmt.Errorf("chmod config directory %s: %w", configDir, err)
	}
	created, err := writeFileExclusive(configPath, data, 0o600)
	if err != nil {
		return false, err
	}
	return created, nil
}

func writeFileExclusive(path string, data []byte, perm os.FileMode) (bool, error) {
	configDir := filepath.Dir(path)
	tempFile, err := os.CreateTemp(configDir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return false, fmt.Errorf("create temp config file in %s: %w", configDir, err)
	}
	tempPath := tempFile.Name()
	if err := tempFile.Chmod(perm); err != nil {
		return false, cleanupTempConfigFile(tempFile, tempPath, fmt.Errorf("chmod temp config file %s: %w", tempPath, err))
	}
	written, err := tempFile.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return false, cleanupTempConfigFile(tempFile, tempPath, fmt.Errorf("write temp config file %s: %w", tempPath, err))
	}
	if err := tempFile.Sync(); err != nil {
		return false, cleanupTempConfigFile(tempFile, tempPath, fmt.Errorf("sync temp config file %s: %w", tempPath, err))
	}
	if err := tempFile.Close(); err != nil {
		return false, cleanupTempPath(tempPath, fmt.Errorf("close temp config file %s: %w", tempPath, err))
	}
	return publishConfigFile(tempPath, path)
}

func publishConfigFile(tempPath string, finalPath string) (bool, error) {
	if err := os.Link(tempPath, finalPath); err != nil {
		cleanupErr := cleanupTempPath(tempPath, nil)
		if os.IsExist(err) {
			if cleanupErr != nil {
				return false, cleanupErr
			}
			return false, nil
		}
		if cleanupErr != nil {
			return false, errors.Join(fmt.Errorf("publish config file %s from %s: %w", finalPath, tempPath, err), cleanupErr)
		}
		return false, fmt.Errorf("publish config file %s from %s: %w", finalPath, tempPath, err)
	}
	if err := syncParentDir(finalPath); err != nil {
		return false, cleanupTempPath(tempPath, fmt.Errorf("sync config directory after publishing %s: %w", finalPath, err))
	}
	if err := os.Remove(tempPath); err != nil {
		return false, fmt.Errorf("remove published temp config file %s: %w", tempPath, err)
	}
	if err := syncParentDir(finalPath); err != nil {
		return false, fmt.Errorf("sync config directory after removing temp config %s: %w", tempPath, err)
	}
	return true, nil
}

func syncParentDir(path string) error {
	dirPath := filepath.Dir(path)
	dir, err := os.Open(dirPath)
	if err != nil {
		return fmt.Errorf("open config directory %s: %w", dirPath, err)
	}
	if err := dir.Sync(); err != nil {
		closeErr := dir.Close()
		if closeErr != nil {
			return errors.Join(fmt.Errorf("sync config directory %s: %w", dirPath, err), fmt.Errorf("close config directory %s: %w", dirPath, closeErr))
		}
		return fmt.Errorf("sync config directory %s: %w", dirPath, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close config directory %s: %w", dirPath, err)
	}
	return nil
}

func cleanupTempConfigFile(file *os.File, path string, cause error) error {
	var errs []error
	errs = append(errs, cause)
	if err := file.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close temp config file %s: %w", path, err))
	}
	return cleanupTempPath(path, errors.Join(errs...))
}

func cleanupTempPath(path string, cause error) error {
	var errs []error
	if cause != nil {
		errs = append(errs, cause)
	}
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove temp config file %s: %w", path, err))
		}
	} else if err := syncParentDir(path); err != nil {
		errs = append(errs, fmt.Errorf("sync config directory after removing temp config %s: %w", path, err))
	}
	return errors.Join(errs...)
}

func writeStartupError(output io.Writer, title string, configPath string, err error, hints ...string) {
	fmt.Fprintf(output, "Provider Bridge failed to start: %s\n", title)
	if configPath != "" {
		fmt.Fprintf(output, "config file: %s\n", configPath)
	}
	fmt.Fprintln(output, "error details:")
	for i, msg := range errorChain(err) {
		fmt.Fprintf(output, "  %d. %s\n", i+1, msg)
	}
	if len(hints) == 0 {
		return
	}
	fmt.Fprintln(output, "suggestions:")
	for _, hint := range hints {
		fmt.Fprintf(output, "  - %s\n", hint)
	}
}

func errorChain(err error) []string {
	if err == nil {
		return []string{"<nil>"}
	}
	var messages []string
	for current := err; current != nil; current = errors.Unwrap(current) {
		messages = append(messages, current.Error())
	}
	return messages
}
