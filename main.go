package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/viper"
)

type Config struct {
	Signatures map[string]string `mapstructure:"signatures"`
}

type Rule struct {
	Name  string
	Regex *regexp.Regexp
}

type ScanJob struct {
	RepoName  string
	CommitUrl string
}

func initViper(cfg *Config) error {
	viper.SetConfigName("config")
	viper.SetConfigType("toml")
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("error loading config %v", err)
		return err
	}

	if err := viper.Unmarshal(&cfg); err != nil {
		log.Fatalf("error unmarshaling config: %v", err)
		return err
	}

	log.Println("Loaded config:")
	for k, v := range cfg.Signatures {
		log.Printf("  - %s: %s\n", k, v)
	}

	return nil
}

func main() {
	// Redirect logger to a file so it doesn't mess up the TUI
	logFile, err := os.OpenFile("scanner.log", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0666)
	if err != nil {
		log.SetOutput(io.Discard)
	} else {
		log.SetOutput(logFile)
		defer logFile.Close()
	}

	var cfg Config
	if err := initViper(&cfg); err != nil {
		log.Printf("failed to init viper: %v", err)
	}

	var rules []Rule
	for name, pattern := range cfg.Signatures {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			log.Fatalf("Invalid regex %s: %w", pattern, err)
		}
		rules = append(rules, Rule{
			Name:  name,
			Regex: compiled,
		})
	}
	log.Printf("Successfully loaded %d rules!\n", len(rules))

	jobs := make(chan ScanJob, 100)
	numWorkers := 100

	initialModel := tuiModel{
		status:        "Initializing...",
		activeWorkers: numWorkers,
	}
	p := tea.NewProgram(initialModel)

	for w := 1; w <= numWorkers; w++ {
		go worker(w, p, jobs, rules)
	}
	log.Printf("Started %d parallel scanning workers.\n", numWorkers)

	go func() {
		client := &http.Client{
			Timeout: 30 * time.Second,
		}
		url := "https://api.github.com/events?per_page=100"
		var lastETag string
		pollInterval := 60 * time.Second

		processedCommits := make(map[string]bool)

		for {
			p.Send(MsgStatusUpdate{Status: "Fetching events..."})
			req, _ := http.NewRequest("GET", url, nil)
			req.Header.Set("User-Agent", "scanner")
			if lastETag != "" {
				req.Header.Add("If-None-Match", lastETag)
			}
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("Error fetching events: %v", err)
				p.Send(MsgStatusUpdate{Status: fmt.Sprintf("Error fetching events: %v", err)})
				time.Sleep(pollInterval)
				continue
			}

			// Parse Rate Limits
			if limitStr := resp.Header.Get("X-Ratelimit-Limit"); limitStr != "" {
				var limit, remain int
				fmt.Sscanf(limitStr, "%d", &limit)
				if remainStr := resp.Header.Get("X-Ratelimit-Remaining"); remainStr != "" {
					fmt.Sscanf(remainStr, "%d", &remain)
					p.Send(MsgRateLimit{Limit: limit, Remaining: remain})
				}
			}

			if resp.StatusCode == http.StatusNotModified {
				resp.Body.Close()
				p.Send(MsgStatusUpdate{Status: "Events not modified."})
				time.Sleep(pollInterval)
				continue
			}
			if resp.StatusCode != http.StatusOK {
				log.Printf("Unexpected status code fetching events: %d", resp.StatusCode)
				p.Send(MsgStatusUpdate{Status: fmt.Sprintf("HTTP %d error", resp.StatusCode)})
				resp.Body.Close()
				time.Sleep(pollInterval)
				continue
			}
			lastETag = resp.Header.Get("ETag")

			var events []map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
				log.Printf("Error unmarshalling events: %v", err)
				resp.Body.Close()
				time.Sleep(pollInterval)
				continue
			}
			resp.Body.Close()

			p.Send(MsgStatusUpdate{Status: "Processing events..."})
			newCommitsCount := 0
			for _, event := range events {
				eventType, _ := event["type"].(string)
				if eventType != "PushEvent" {
					continue
				}

				repo, _ := event["repo"].(map[string]any)
				repoName, _ := repo["name"].(string)
				payload, _ := event["payload"].(map[string]any)

				sha, ok := payload["head"].(string)
				if !ok || sha == "" {
					continue
				}

				if processedCommits[sha] {
					continue
				}
				processedCommits[sha] = true
				newCommitsCount++

				patchUrl := fmt.Sprintf("https://api.github.com/repos/%s/commits/%s", repoName, sha)
				jobs <- ScanJob{
					RepoName:  repoName,
					CommitUrl: patchUrl,
				}
			}
			p.Send(MsgFetchedCommits{Count: newCommitsCount})
			p.Send(MsgStatusUpdate{Status: fmt.Sprintf("Fetched %d events (%d new push commits)", len(events), newCommitsCount)})

			if intervalStr := resp.Header.Get("X-Poll-Interval"); intervalStr != "" {
				if duration, err := time.ParseDuration(intervalStr + "s"); err == nil {
					pollInterval = duration
				}
			}

			time.Sleep(pollInterval)
		}
	}()

	if _, err := p.Run(); err != nil {
		log.Printf("Error running TUI: %v\n", err)
		os.Exit(1)
	}
}

func worker(id int, p *tea.Program, jobs <-chan ScanJob, rules []Rule) {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	for job := range jobs {
		p.Send(MsgScanStarted{CommitUrl: job.CommitUrl})
		req, err := http.NewRequest("GET", job.CommitUrl, nil)
		if err != nil {
			p.Send(MsgScanCompleted{CommitUrl: job.CommitUrl})
			continue
		}
		req.Header.Set("User-Agent", "scanner")
		req.Header.Set("Accept", "application/vnd.github.v3.diff")

		resp, err := client.Do(req)
		if err != nil {
			p.Send(MsgScanCompleted{CommitUrl: job.CommitUrl})
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			p.Send(MsgScanCompleted{CommitUrl: job.CommitUrl})
			continue
		}

		scanner := bufio.NewScanner(resp.Body)
		lineNumber := 0

		for scanner.Scan() {
			lineNumber++
			lineText := scanner.Text()

			if len(lineText) > 0 && lineText[0] == '+' && (len(lineText) < 3 || lineText[:3] != "+++") {
				for _, rule := range rules {
					if rule.Regex.MatchString(lineText) {
						p.Send(MsgMatchFound{
							Repo:      job.RepoName,
							CommitUrl: job.CommitUrl,
							Rule:      rule.Name,
							Line:      lineNumber,
							Text:      lineText,
						})
					}
				}
			}
		}
		resp.Body.Close()
		p.Send(MsgScanCompleted{CommitUrl: job.CommitUrl})
	}
}

// Bubble Tea TUI Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 1).
			MarginBottom(1)

	accentStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7D56F4")).
			Bold(true)

	greenStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#04B575")).
			Bold(true)

	redStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF5555")).
			Bold(true)

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#874BFD")).
			Padding(1).
			MarginBottom(1).
			Width(60)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6272A4")).
			Italic(true)
)

// TUI messages
type MsgFetchedCommits struct {
	Count int
}

type MsgScanStarted struct {
	CommitUrl string
}

type MsgScanCompleted struct {
	CommitUrl string
}

type MsgMatchFound struct {
	Repo      string
	CommitUrl string
	Rule      string
	Line      int
	Text      string
}

type MsgRateLimit struct {
	Remaining int
	Limit     int
}

type MsgStatusUpdate struct {
	Status string
}

// Bubble Tea Model
type tuiModel struct {
	totalFound      int
	totalScanned    int
	totalHits       int
	recentHits      []MsgMatchFound
	status          string
	rateLimitLimit  int
	rateLimitRemain int
	activeWorkers   int
}

func (m tuiModel) Init() tea.Cmd {
	return nil
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case MsgFetchedCommits:
		m.totalFound += msg.Count

	case MsgScanStarted:
		m.status = fmt.Sprintf("Scanning commit: %s", msg.CommitUrl)

	case MsgScanCompleted:
		m.totalScanned++

	case MsgMatchFound:
		m.totalHits++
		m.recentHits = append(m.recentHits, msg)
		if len(m.recentHits) > 5 {
			m.recentHits = m.recentHits[len(m.recentHits)-5:]
		}

	case MsgRateLimit:
		m.rateLimitRemain = msg.Remaining
		m.rateLimitLimit = msg.Limit

	case MsgStatusUpdate:
		m.status = msg.Status
	}

	return m, nil
}

func (m tuiModel) View() string {
	var s string

	// Title
	s += titleStyle.Render("Commit scanner") + "\n\n"

	// Stats content
	var stats string
	stats += fmt.Sprintf("Status: %s\n", accentStyle.Render(m.status))
	stats += fmt.Sprintf("Found: %d commits\n", m.totalFound)
	stats += fmt.Sprintf("Scanned: %d commits\n", m.totalScanned)
	stats += fmt.Sprintf("Matches/Hits: %s\n", redStyle.Render(fmt.Sprintf("%d", m.totalHits)))
	stats += fmt.Sprintf("Workers: %d active\n", m.activeWorkers)

	rlStr := "N/A"
	if m.rateLimitLimit > 0 {
		rlStr = fmt.Sprintf("%d/%d", m.rateLimitRemain, m.rateLimitLimit)
		if m.rateLimitRemain < 10 {
			rlStr = redStyle.Render(rlStr)
		} else {
			rlStr = greenStyle.Render(rlStr)
		}
	}
	stats += fmt.Sprintf("Rate Limit:   %s\n", rlStr)

	s += borderStyle.Render(stats) + "\n"

	// Recent matches
	s += accentStyle.Render("Recent Matches:") + "\n"
	if len(m.recentHits) == 0 {
		s += "  No matches found yet.\n"
	} else {
		for _, hit := range m.recentHits {
			s += fmt.Sprintf("  [%s] Rule: %s in %s\n", redStyle.Render("MATCH"), greenStyle.Render(hit.Rule), hit.Repo)
			s += fmt.Sprintf("  ↳ Commit: %s\n", hit.CommitUrl)
			txt := hit.Text
			if len(txt) > 60 {
				txt = txt[:57] + "..."
			}
			s += fmt.Sprintf("  ↳ Line %d: %s\n\n", hit.Line, txt)
		}
	}

	s += "\n" + helpStyle.Render("Press 'q' or 'Ctrl+C' to exit.") + "\n"
	return s
}
