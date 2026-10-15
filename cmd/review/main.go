package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"strings"
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

func main() {
	// Support: `ai-ded-pr diff` and `ai-ded-pr branch-diff <branch> <base>`
	if len(os.Args) > 1 && (os.Args[1] == "diff" || os.Args[1] == "branch-diff") {
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
	dumpFlag := flag.Bool("dump", false, "Load PR, print review plan, exit")
	aiDumpFlag := flag.Bool("ai-dump", false, "Use local model in dump (requires --model or default model)")
	flag.Parse()

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
		opts = append(opts, tui.Options{Mode: 0, Source: *githubFlag, AutoLoad: true})
	} else {
		if *repoFlag == "" {
			*repoFlag = "."
		}
		opts = append(opts, tui.Options{Mode: 1, Source: *repoFlag, Base: *baseFlag, Head: *headFlag, AutoLoad: true})
	}

	if *dumpFlag {
		ctx := context.Background()
		var pr *models.PullRequest
		var err error
		if *githubFlag != "" {
			ref, e := github.MakeRefFromURL(*githubFlag)
			if e != nil {
				fmt.Println(e)
				return
			}
			pr, err = github.New("").LoadPR(ctx, ref)
		} else {
			ad := localgit.New(*repoFlag, *baseFlag, *headFlag)
			pr, err = ad.Load(ctx)
		}
		if err != nil {
			fmt.Println("load err:", err)
			return
		}
		fmt.Printf("PR: %s (+%d/-%d) files=%d\n", pr.Title, pr.TotalAdd, pr.TotalDel, len(pr.Files))
		var client *modelsvc.Client
		if *aiDumpFlag {
			mp := cfg.LLMModel
			if *modelFlag != "" {
				mp = *modelFlag
			}
			if _, err := os.Stat(mp); err == nil {
				srv, err := modelsvc.StartServer(ctx, "llama-server", mp, 18993, 4096, false)
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
		}
		fmt.Println("Summary:", plan.Summary)
		// test file summary for first file
		if len(pr.Files) > 0 {
			fmt.Println("--- file summary (first changed) ---")
			fmt.Println(planner.FileSummary(ctx, &pr.Files[0]))
		}
		return
	}

	m := tui.New(cfg, *noAIFlag, opts...)
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.SetProgram(p)

	// Ensure clean shutdown (kill llama-server children) on Ctrl+C / SIGTERM.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		m.Shutdown()
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
