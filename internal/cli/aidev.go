package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/internal/assistant"
)

// devModeNotice is what `tobira ai dev` prints once the flows are registered.
const devModeNotice = "Genkit dev mode: run `genkit start -- tobira ai dev`"

// newAICmd is the hidden `ai` command group. Its only command, `ai dev`,
// is a developer tool: it serves the assistant's flows to the Genkit Dev UI.
func (a *app) newAICmd() *cobra.Command {
	group := &cobra.Command{
		Use:    "ai",
		Short:  "AI assistant developer tools",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	dev := &cobra.Command{
		Use:   "dev",
		Short: "Serve the assistant's flows to the Genkit Dev UI",
		Long: "Initialise the assistant in Genkit dev mode with every flow registered, and\n" +
			"block until interrupted. Attach the Dev UI with:\n\n" +
			"  genkit start -- tobira ai dev\n\n" +
			"genkit start sets GENKIT_ENV=dev, which starts Genkit's reflection server.\n\n" +
			"While it runs, any local process can call the flows through that server on\n" +
			"127.0.0.1: they run read queries on every configured connection and, as\n" +
			"ai.send_rows allows, send rows to the provider. Genkit may also store traces\n" +
			"of each run, prompts and rows included, under .genkit/ in the working\n" +
			"directory. Run it only with test connections (or send_rows: never).",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE:   a.runAIDev,
	}
	group.AddCommand(dev)
	return group
}

func (a *app) runAIDev(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	debug, _ := cmd.Flags().GetBool("debug")
	log, closeLog, err := a.openLog(debug)
	if err != nil {
		return usageError(err)
	}
	defer closeLog()
	cfg, err := a.loadConfig(cmd)
	if err != nil {
		return err
	}
	if cfg.AI == nil || a.deps.NewAssistant == nil {
		return usageError(fmt.Errorf("%w: add an ai: section to the config file", assistant.ErrNotConfigured))
	}
	mgr, _ := a.newManagerMode(ctx, cfg, false, log, true)
	defer func() { _ = mgr.CloseAll() }()
	hist := a.openHistory(ctx, log, cfg)
	defer func() { _ = hist.Close() }()
	// Every plugin connection's real Info, so the flows' tools see its
	// capabilities; one that does not answer is logged and its tools fail.
	unavailable, err := a.resolvePlugins(ctx, mgr, mgr.Names(), log)
	if err != nil {
		return nil // a signal while starting
	}
	for conn, why := range unavailable {
		log.Warn("connection unavailable", "connection", conn, "reason", why)
	}
	if _, err := a.buildAssistantMode(ctx, cfg, mgr, hist, log, true); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(a.streams.Out, devModeNotice)
	<-ctx.Done()
	return nil
}
