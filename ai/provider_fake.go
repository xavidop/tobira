//go:build aifake

package ai

import (
	"context"
	"encoding/json"
	"os"

	gai "github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core/api"
	"github.com/xavidop/tobira/ai/aitest"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
)

// fakeScriptEnv names the JSON script the fake provider answers from.
const fakeScriptEnv = "TOBIRA_AI_FAKE_SCRIPT"

// fakeCaptureEnv, when set, names a file that gets one JSON line per model
// request, so an end-to-end test of the binary can see what would have been
// sent to a provider.
const fakeCaptureEnv = "TOBIRA_AI_FAKE_CAPTURE"

// fakeProvider is the scripted test provider, compiled in only with -tags
// aifake for the end-to-end smoke test: ai.provider: fake answers from the
// aitest script in $TOBIRA_AI_FAKE_SCRIPT and never touches the network.
func fakeProvider(name string) (provider, bool) {
	if name != aitest.Provider {
		return provider{}, false
	}
	return provider{
		defaultModel: "scripted",
		plugin: func(cfg config.AI, _ driver.Secret, getenv func(string) string) api.Plugin {
			path := getenv(fakeScriptEnv)
			if path == "" {
				panic("set " + fakeScriptEnv + " to the script of the fake provider")
			}
			script, err := aitest.LoadScript(path)
			if err != nil {
				panic(err)
			}
			if capture := getenv(fakeCaptureEnv); capture != "" {
				for i := range script {
					script[i].Before = captureRequest(capture)
				}
			}
			return &aitest.Plugin{Model: cfg.Model, Fake: aitest.NewModel(script...)}
		},
	}, true
}

// captureRequest appends each request, as one JSON line, to path.
func captureRequest(path string) func(context.Context, *gai.ModelRequest) error {
	return func(_ context.Context, req *gai.ModelRequest) error {
		line, err := json.Marshal(req)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = f.Write(append(line, '\n'))
		return err
	}
}
