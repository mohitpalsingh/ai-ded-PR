package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mohit/ai-ded-pr/internal/config"
	"github.com/mohit/ai-ded-pr/internal/core/models"
	"github.com/mohit/ai-ded-pr/internal/core/review"
	"github.com/mohit/ai-ded-pr/internal/hosts/github"
	"github.com/mohit/ai-ded-pr/internal/hosts/localgit"
	"github.com/mohit/ai-ded-pr/internal/modelsvc"
	"github.com/mohit/ai-ded-pr/internal/tui"
)

// version is stamped via -ldflags "-X main.version=..." (see Makefile).
var version = "dev"

func main() {
	// Support: `ai-ded-pr diff` and `ai-ded-pr branch-diff <branch> <base>`
	if len(os.Args) > 1 && (os.Args[1] == "diff" || os.Args[1] == "branch-diff" || os.Args[1] == "ask") {
		if os.Args[1] == "branch-diff" {
			args := []string{os.Args[0]}
			if len(os.Args) >= 4 {
				// ai-ded-pr branch-diff feature/xyz master
				args = append(args, "-repo", ".")
				args = append(args, "-base", os.Args[3])
				args = append(args, "-head", os.Args[2])
				args = append(args, os.Args[4:]...)
			}
			os.Args = args
		} else if os.Args[1] == "ask" {
			args := []string{os.Args[0], "-ask"}
			if len(os.Args) >= 3 {
				args = append(args, "-q", os.Args[2])
				args = append(args, os.Args[3:]...)
			}
			os.Args = args
		} else {
			os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		}
	}
	githubFlag := flag.String("github", "", "GitHub PR URL")
	repoFlag := flag.String("repo", "", "Local git repo path")
	baseFlag := flag.String("base", "HEAD~1", "Base ref for local diff")
	headFlag := flag.String("head", "HEAD", "Head ref for local diff")
	modelFlag := flag.String("model", "", "Path to GGUF model")
	noAIFlag := flag.Bool("no-ai", false, "Disable local AI (heuristic only)")
	askFlag := flag.Bool("ask", false, "Ask a question over a PR/repo and print relevant files")
	queryFlag := flag.String("q", "", "Question for --ask")
	dumpFlag := flag.Bool("dump", false, "Load PR, print review plan, exit")
	aiDumpFlag := flag.Bool("ai-dump", false, "Use local model in dump (requires --model or default model)")
	versionFlag := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Println("ai-ded-pr", version)
		return
	}

	// Make plain `ai-ded-pr diff` work with no extra params: review current repo.
	if *githubFlag == "" && *repoFlag == "" {
		*repoFlag = "."
	}

	cfg := config.Default()
	if *modelFlag != "" {
		cfg.LLMModel = *modelFlag
	}
	if err := config.EnsureDir(cfg); err != nil {
		fmt.Println("config dir error:", err)
		os.Exit(1)
	}

	var opts []tui.Options
	if *githubFlag != "" {
		opts = append(opts, tui.Options{Mode: 0, Source: *githubFlag, AutoLoad: true, Version: version})
	} else {
		if *repoFlag == "" {
			*repoFlag = "."
		}
		opts = append(opts, tui.Options{Mode: 1, Source: *repoFlag, Base: *baseFlag, Head: *headFlag, AutoLoad: true, Version: version})
	}

	if *askFlag {
		if *queryFlag == "" {
			fmt.Println("usage: ai-ded-pr ask \"question\"")
			return
		}
		ctx := context.Background()
		var pr *models.PullRequest
		var err error
		if *githubFlag != "" {
			ref, e := github.MakeRefFromURL(*githubFlag)
			if e != nil {
				fmt.Println(e)
				os.Exit(1)
			}
			pr, err = github.New("").LoadPR(ctx, ref)
		} else {
			if *repoFlag == "" {
				*repoFlag = "."
			}
			ad := localgit.New(*repoFlag, *baseFlag, *headFlag)
			pr, err = ad.Load(ctx)
		}
		if err != nil {
			fmt.Println("load err:", err)
			os.Exit(1)
		}
		var client *modelsvc.Client
		var embedClient *modelsvc.Client
		if !*noAIFlag {
			mp := cfg.LLMModel
			if *modelFlag != "" {
				mp = *modelFlag
			}
			if _, err := os.Stat(mp); err == nil {
				srv, err := modelsvc.StartServer(ctx, "llama-server", mp, 18993, 8192, false)
				if err == nil {
					client = modelsvc.NewClient(srv.URL)
					for i := 0; i < 60 && !client.Healthy(ctx); i++ {
						time.Sleep(500 * time.Millisecond)
					}
					defer srv.Stop()
				}
			}
			if _, err := os.Stat(cfg.EmbedModel); err == nil {
				esrv, err := modelsvc.StartEmbeddingServer(ctx, "llama-server", cfg.EmbedModel, 18994, 2048)
				if err == nil {
					embedClient = modelsvc.NewClient(esrv.URL)
					defer esrv.Stop()
				}
			}
		}
		q := *queryFlag
		planner := review.NewPlanner(client, embedClient)
		// One planning call annotates every file (no per-file LLM calls).
		var intents map[string]string
		if client != nil {
			if plan, err := planner.Plan(ctx, pr); err == nil {
				intents = map[string]string{}
				for _, s := range plan.Steps {
					for _, ln := range strings.Split(s.Rationale, "\n") {
						if strings.HasPrefix(ln, "- Intent: ") {
							intents[s.Path] = strings.TrimPrefix(ln, "- Intent: ")
							break
						}
					}
				}
			}
		}
		res := planner.Search(ctx, pr, q)
		fmt.Printf("Question: %s\n", q)
		fmt.Printf("Relevant files (%d):\n", len(res))
		for _, r := range res {
			fmt.Println("  -", r)
			if t := intents[r]; t != "" {
				fmt.Println("      " + t)
			}
		}
		return
	}

	if *dumpFlag {
		ctx := context.Background()
		var pr *models.PullRequest
		var err error
		if *githubFlag != "" {
			ref, e := github.MakeRefFromURL(*githubFlag)
			if e != nil {
				fmt.Println(e)
				os.Exit(1)
			}
			pr, err = github.New("").LoadPR(ctx, ref)
		} else {
			ad := localgit.New(*repoFlag, *baseFlag, *headFlag)
			pr, err = ad.Load(ctx)
		}
		if err != nil {
			fmt.Println("load err:", err)
			os.Exit(1)
		}
		fmt.Printf("PR: %s (+%d/-%d) files=%d\n", pr.Title, pr.TotalAdd, pr.TotalDel, len(pr.Files))
		var client *modelsvc.Client
		if *aiDumpFlag {
			mp := cfg.LLMModel
			if *modelFlag != "" {
				mp = *modelFlag
			}
			if _, err := os.Stat(mp); err == nil {
				srv, err := modelsvc.StartServer(ctx, "llama-server", mp, 18993, 8192, false)
				if err == nil {
					client = modelsvc.NewClient(srv.URL)
					defer srv.Stop()
					// wait healthy
					for i := 0; i < 60 && !client.Healthy(ctx); i++ {
						time.Sleep(500 * time.Millisecond)
					}
				} else {
					fmt.Println("model server err:", err)
				}
			} else {
				fmt.Println("model not found:", mp)
			}
		}
		var embedClient *modelsvc.Client
		if *aiDumpFlag && client != nil {
			if _, err := os.Stat(cfg.EmbedModel); err == nil {
				srv, err := modelsvc.StartEmbeddingServer(ctx, "llama-server", cfg.EmbedModel, 18994, 2048)
				if err == nil {
					embedClient = modelsvc.NewClient(srv.URL)
					for i := 0; i < 60 && !embedClient.Healthy(ctx); i++ {
						time.Sleep(500 * time.Millisecond)
					}
					defer srv.Stop()
				}
			}
		}
		planner := review.NewPlanner(client, embedClient)
		plan, _ := planner.Plan(ctx, pr)
		for _, s := range plan.Steps {
			fmt.Printf("%2d %-10s %s\n", s.Order, s.Status, s.Path)
			if s.Rationale != "" {
				fmt.Printf("     rationale: %s\n", s.Rationale)
			}
			if len(s.Focus) > 0 {
				fmt.Printf("     focus: %s\n", strings.Join(s.Focus, "; "))
			}
			if len(s.DependsOn) > 0 {
				fmt.Printf("     deps: %s\n", strings.Join(s.DependsOn, "; "))
			}
		}
		fmt.Println("Summary:", plan.Summary)
		return
	}

	m := tui.New(cfg, *noAIFlag, opts...)
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.SetProgram(p)

	// Handle Ctrl+C / SIGTERM / SIGHUP without leaving llama-server children behind.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		<-sigCh
		m.Shutdown()
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		m.Shutdown()
		fmt.Println("error:", err)
		os.Exit(1)
	}
	m.Shutdown()
}
