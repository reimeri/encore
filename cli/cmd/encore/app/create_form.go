package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/tailscale/hujson"

	"encr.dev/cli/cmd/encore/cmdutil"
)

const (
	codeBlue       = "#6D89FF"
	codePurple     = "#A36C8C"
	codeGreen      = "#B3D77E"
	validationFail = "#CB1010"
)

var (
	inputStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Dark: codeBlue, Light: codeBlue})
	descStyle    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Dark: codeGreen, Light: codePurple})
	docStyle     = lipgloss.NewStyle().Padding(0, 2, 0, 2)
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(validationFail))
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#00C200"))
)

type templateItem struct {
	ItemTitle string   `json:"title"`
	Desc      string   `json:"desc"`
	Template  string   `json:"template"`
	Lang      language `json:"lang"`
}

func (i templateItem) Title() string       { return i.ItemTitle }
func (i templateItem) Description() string { return i.Desc }
func (i templateItem) FilterValue() string { return i.ItemTitle }

type createFormModel struct {
	step int // 0, 1, 2, 3

	lang      languageSelectModel
	templates templateListModel
	appName   appNameModel

	skipShowingTemplate bool

	aborted bool
}

func (m createFormModel) Init() tea.Cmd {
	return tea.Batch(
		m.appName.Init(),
		m.templates.Init(),
	)
}

type languageSelectDone struct {
	lang language
}

type languageSelectModel struct {
	list list.Model
}

func (m languageSelectModel) Selected() language {
	sel := m.list.SelectedItem()
	if sel == nil {
		return ""
	}
	return sel.(langItem).lang
}

func (m languageSelectModel) Update(msg tea.Msg) (languageSelectModel, tea.Cmd) {
	var c tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			// Have we selected a language?
			if idx := m.list.Index(); idx >= 0 {
				return m, func() tea.Msg {
					return languageSelectDone{
						lang: m.Selected(),
					}
				}
			}
		}
	}

	m.list, c = m.list.Update(msg)
	return m, c
}

func (m *languageSelectModel) SetSize(width, height int) {
	m.list.SetWidth(width)
	m.list.SetHeight(max(height-1, 0))
}

const checkmark = "✔"

func (m languageSelectModel) View() string {
	var b strings.Builder
	b.WriteString(inputStyle.Render("Select language for your application"))
	b.WriteString(descStyle.Render(" [Use arrows to move]"))
	b.WriteString("\n")
	b.WriteString(m.list.View())

	return b.String()
}

type appNameDone struct{}

type appNameModel struct {
	predefined string
	text       textinput.Model
	dirExists  bool
}

func (m appNameModel) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
	)
}

func (m appNameModel) Selected() string {
	if m.predefined != "" {
		return m.predefined
	}
	return m.text.Value()
}

func (m appNameModel) Update(msg tea.Msg) (appNameModel, tea.Cmd) {
	var cmds []tea.Cmd
	var c tea.Cmd
	m.text, c = m.text.Update(msg)
	cmds = append(cmds, c)

	if val := m.text.Value(); val != "" {
		_, err := os.Stat(val)
		m.dirExists = err == nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			if m.text.Value() != "" && !m.dirExists {
				cmds = append(cmds, func() tea.Msg {
					return appNameDone{}
				})
			}
		}
	}

	return m, tea.Batch(cmds...)
}

func (m appNameModel) View() string {
	var b strings.Builder
	if m.text.Focused() {
		b.WriteString(inputStyle.Render("App Name"))
		b.WriteString(descStyle.Render(" [Use only lowercase letters, digits, and dashes]"))
		b.WriteByte('\n')
		b.WriteString(m.text.View())
		if m.dirExists {
			b.WriteString(errorStyle.Render(" error: dir already exists"))
		}
	} else {
		fmt.Fprintf(&b, "%s App Name: %s", checkmark, m.text.Value())
	}
	b.WriteByte('\n')
	return b.String()
}

type templateListModel struct {
	predefined string
	filter     language

	all     []templateItem
	list    list.Model
	loading spinner.Model
}

func (m templateListModel) Init() tea.Cmd {
	return tea.Batch(
		loadTemplates,
		m.loading.Tick,
	)
}

func (m *templateListModel) SetSize(width, height int) {
	m.list.SetWidth(width)
	m.list.SetHeight(max(height-1, 0))
}

type templateSelectDone struct{}

func (m templateListModel) Update(msg tea.Msg) (templateListModel, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			// Have we selected a language?
			if idx := m.list.Index(); idx >= 0 {
				return m, func() tea.Msg { return templateSelectDone{} }
			}
		}

	case spinner.TickMsg:
		m.loading, _ = m.loading.Update(msg)

	case loadedTemplates:
		m.all = msg
		m.refreshFilter()
		newList, c := m.list.Update(msg)
		m.list = newList
		cmds = append(cmds, c)
	}

	newList, c := m.list.Update(msg)
	m.list = newList
	cmds = append(cmds, c)

	return m, tea.Batch(cmds...)
}

func (m *templateListModel) UpdateFilter(lang language) {
	m.filter = lang
	m.refreshFilter()
}

func (m *templateListModel) refreshFilter() {
	var listItems []list.Item
	for _, it := range m.all {
		if it.Lang == m.filter {
			listItems = append(listItems, it)
		}
	}
	m.list.SetItems(listItems)
}

func (m templateListModel) View() string {
	var b strings.Builder
	b.WriteString(inputStyle.Render("Template"))
	b.WriteString(descStyle.Render(" [Use arrows to move]"))
	b.WriteByte('\n')
	b.WriteString(m.list.View())

	return b.String()
}

func (m templateListModel) Selected() string {
	if m.predefined != "" {
		return m.predefined
	}
	idx := m.list.Index()
	if idx < 0 {
		return ""
	}
	return m.list.Items()[idx].FilterValue()
}

func (m createFormModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		cmds []tea.Cmd
		c    tea.Cmd
	)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc, 'q':
			m.aborted = true
			return m, tea.Quit
		}

		switch m.step {
		case 0:
			m.lang, c = m.lang.Update(msg)
			cmds = append(cmds, c)
		case 1:
			m.templates, c = m.templates.Update(msg)
			cmds = append(cmds, c)
		case 2:
			m.appName, c = m.appName.Update(msg)
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case languageSelectDone:
		m.step = 1
		if m.skipShowingTemplate {
			m.step = 2
		}
		m.templates.UpdateFilter(msg.lang)

	case templateSelectDone:
		if m.appName.predefined != "" {
			// We're done.
			m.step = 3
			cmds = append(cmds, tea.Quit)
		} else {
			m.step = 2
		}

	case appNameDone:
		cmds = append(cmds, tea.Quit)
		m.step = 3

	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil
	}

	// Update all submodels for other messages.
	m.lang, c = m.lang.Update(msg)
	cmds = append(cmds, c)
	m.templates, c = m.templates.Update(msg)
	cmds = append(cmds, c)
	m.appName, c = m.appName.Update(msg)
	cmds = append(cmds, c)

	return m, tea.Batch(cmds...)
}

func (m *createFormModel) SetSize(width, height int) {
	// Step 1
	doneHeight := lipgloss.Height(m.doneView())
	{
		availHeight := height - doneHeight
		m.lang.SetSize(width, availHeight)
	}

	// Step 2
	{
		availHeight := height - doneHeight
		m.templates.SetSize(width, availHeight)
	}
}

func (m createFormModel) doneView() string {
	var b strings.Builder

	renderDone := func(title, value string) {
		b.WriteString(successStyle.Render(fmt.Sprintf("%s %s: ", checkmark, title)))
		b.WriteString(value)
		b.WriteByte('\n')
	}

	renderLangDone := func() {
		renderDone("Language", m.lang.Selected().Display())
	}

	renderNameDone := func() {
		renderDone("App Name", m.appName.Selected())
	}

	renderTemplateDone := func() {
		renderDone("Template", m.templates.Selected())
	}

	if m.appName.predefined != "" {
		renderNameDone()
	}
	if m.templates.predefined == "" && m.step > 0 {
		renderLangDone()
	}
	if !m.skipShowingTemplate {
		if m.templates.predefined != "" || m.step > 1 {
			renderTemplateDone()
		}
	}
	if m.appName.predefined == "" && m.step > 2 {
		renderNameDone()
	}

	return b.String()
}

func (m createFormModel) View() string {
	var b strings.Builder

	doneView := m.doneView()

	b.WriteString(doneView)
	if doneView != "" {
		b.WriteByte('\n')
	}

	if m.step == 0 {
		b.WriteString(m.lang.View())
	}

	if m.step == 1 {
		b.WriteString(m.templates.View())
	}

	if m.step == 2 {
		b.WriteString(m.appName.View())
	}

	return docStyle.Render(b.String())
}

func (m templateListModel) templatesLoading() bool {
	return len(m.list.Items()) == 0
}

func (m templateListModel) SelectedItem() (templateItem, bool) {
	if m.predefined != "" {
		return templateItem{}, false
	}
	idx := m.list.Index()
	items := m.list.Items()
	if idx >= 0 && len(items) > idx {
		return items[idx].(templateItem), true
	}
	return templateItem{}, false
}

func selectTemplate(inputName, inputTemplate string, skipShowingTemplate bool) (appName, template string, selectedLang language) {
	// If we have both name and template already, return them.
	if inputName != "" && inputTemplate != "" {
		return inputName, inputTemplate, ""
	}

	var lang languageSelectModel
	{
		ls := list.NewDefaultItemStyles()
		ls.SelectedTitle = ls.SelectedTitle.Foreground(lipgloss.Color(codeBlue)).BorderForeground(lipgloss.Color(codeBlue))
		ls.SelectedDesc = ls.SelectedDesc.Foreground(lipgloss.Color(codeBlue)).BorderForeground(lipgloss.Color(codeBlue))
		del := list.NewDefaultDelegate()
		del.Styles = ls
		del.ShowDescription = false
		del.SetSpacing(0)

		items := []list.Item{
			langItem{
				lang: languageGo,
				desc: "Build performant and scalable backends with Go",
			},
			langItem{
				lang: languageTS,
				desc: "Build backend and full-stack applications with TypeScript",
			},
		}

		ll := list.New(items, del, 0, 0)
		ll.SetShowTitle(false)
		ll.SetShowHelp(false)
		ll.SetShowPagination(true)
		ll.SetShowFilter(false)
		ll.SetFilteringEnabled(false)
		ll.SetShowStatusBar(false)
		lang = languageSelectModel{
			list: ll,
		}
		lang.SetSize(0, 20)
	}

	var templates templateListModel
	{
		ls := list.NewDefaultItemStyles()
		ls.SelectedTitle = ls.SelectedTitle.Foreground(lipgloss.Color(codeBlue)).BorderForeground(lipgloss.Color(codeBlue))
		ls.SelectedDesc = ls.SelectedDesc.Foreground(lipgloss.Color(codeBlue)).BorderForeground(lipgloss.Color(codeBlue))
		del := list.NewDefaultDelegate()
		del.Styles = ls

		ll := list.New(nil, del, 0, 20)
		ll.SetShowTitle(false)
		ll.SetShowHelp(false)
		ll.SetShowPagination(true)
		ll.SetShowFilter(false)
		ll.SetFilteringEnabled(false)
		ll.SetShowStatusBar(false)

		sp := spinner.New()
		sp.Spinner = spinner.Dot
		sp.Style = inputStyle.Copy().Inline(true)
		templates = templateListModel{
			predefined: inputTemplate,
			list:       ll,
			loading:    sp,
		}
	}

	var nameModel appNameModel
	{
		text := textinput.New()
		text.Focus()
		text.CharLimit = 20
		text.Width = 30
		text.Validate = incrementalValidateNameInput

		nameModel = appNameModel{predefined: inputName, text: text}
	}

	m := createFormModel{
		step:                0,
		lang:                lang,
		templates:           templates,
		appName:             nameModel,
		skipShowingTemplate: skipShowingTemplate,
	}

	// If we have a name, start the list without any selection.
	if m.appName.predefined != "" {
		m.templates.list.Select(-1)
	}
	if m.templates.predefined != "" {
		m.step = 2 // skip to app name selection
	}

	p := tea.NewProgram(m)

	result, err := p.Run()
	if err != nil {
		cmdutil.Fatal(err)
	}

	// Validate the result.
	res := result.(createFormModel)
	if res.aborted {
		os.Exit(1)
	}

	appName, template = inputName, inputTemplate

	if appName == "" {
		appName = res.appName.text.Value()
	}

	if template == "" {
		sel, ok := res.templates.SelectedItem()
		if !ok {
			cmdutil.Fatal("no template selected")
		}
		template = sel.Template
	}

	return appName, template, res.lang.Selected()
}

type langItem struct {
	lang language
	desc string
}

func (i langItem) FilterValue() string {
	return i.lang.Display()
}
func (i langItem) Title() string {
	return i.FilterValue()
}
func (i langItem) Description() string { return "" }

type language string

const (
	languageGo language = "go"
	languageTS language = "ts"
)

func (lang language) Display() string {
	switch lang {
	case languageGo:
		return "Go"
	case languageTS:
		return "TypeScript"
	default:
		return string(lang)
	}
}

type loadedTemplates []templateItem

var defaultTutorials = []templateItem{
	{
		ItemTitle: "Intro to Encore.ts",
		Desc:      "An interactive tutorial",
		Template:  "ts/introduction",
		Lang:      "ts",
	},
}

var defaultTemplates = []templateItem{
	{
		ItemTitle: "Hello World",
		Desc:      "A simple REST API",
		Template:  "hello-world",
		Lang:      "go",
	},
	{
		ItemTitle: "Hello World",
		Desc:      "A simple REST API",
		Template:  "ts/hello-world",
		Lang:      "ts",
	},
	{
		ItemTitle: "Uptime Monitor",
		Desc:      "Microservices, SQL Databases, Pub/Sub, Cron Jobs",
		Template:  "uptime",
		Lang:      "go",
	},
	{
		ItemTitle: "Uptime Monitor",
		Desc:      "Microservices, SQL Databases, Pub/Sub, Cron Jobs",
		Template:  "ts/uptime",
		Lang:      "ts",
	},
	{
		ItemTitle: "GraphQL",
		Desc:      "GraphQL API, Microservices, SQL Database",
		Template:  "graphql",
		Lang:      "go",
	},
	{
		ItemTitle: "URL Shortener",
		Desc:      "REST API, SQL Database",
		Template:  "url-shortener",
		Lang:      "go",
	},
	{
		ItemTitle: "URL Shortener",
		Desc:      "REST API, SQL Database",
		Template:  "ts/url-shortener",
		Lang:      "ts",
	},
	{
		ItemTitle: "SaaS Starter",
		Desc:      "Complete app with Clerk auth, Stripe billing, etc. (advanced)",
		Template:  "ts/saas-starter",
		Lang:      "ts",
	},
	{
		ItemTitle: "Empty app",
		Desc:      "Start from scratch (experienced users only)",
		Template:  "",
		Lang:      "go",
	},
	{
		ItemTitle: "Empty app",
		Desc:      "Start from scratch (experienced users only)",
		Template:  "ts/empty",
		Lang:      "ts",
	},
}

func fetchTemplates(url string, defaults []templateItem) []templateItem {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if req, err := http.NewRequestWithContext(ctx, "GET", url, nil); err == nil {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			if data, err := io.ReadAll(resp.Body); err == nil {
				data, err = hujson.Standardize(data)
				if err == nil {
					var items []templateItem
					if err := json.Unmarshal(data, &items); err == nil && len(items) > 0 {
						return items
					}
				}
			}
		}
	}
	return defaults
}

func loadTemplates() tea.Msg {
	var wg sync.WaitGroup
	var templates, tutorials []templateItem
	wg.Add(1)
	go func() {
		defer wg.Done()
		templates = fetchTemplates("https://raw.githubusercontent.com/encoredev/examples/main/cli-templates.json", defaultTemplates)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		tutorials = fetchTemplates("https://raw.githubusercontent.com/encoredev/examples/main/cli-tutorials.json", defaultTutorials)
	}()
	wg.Wait()
	return loadedTemplates(append(tutorials, templates...))
}

// incrementalValidateNameInput is like validateName but only
// checks for valid/invalid characters. It can't check for
// whether the last character is a dash, since if we treat that
// as an error the user won't be able to enter dashes at all.
func incrementalValidateNameInput(name string) error {
	ln := len(name)
	if ln == 0 {
		return fmt.Errorf("name must not be empty")
	} else if ln > 50 {
		return fmt.Errorf("name too long (max 50 chars)")
	}

	for i, s := range name {
		// Outside of [a-z], [0-9] and != '-'?
		if !((s >= 'a' && s <= 'z') || (s >= '0' && s <= '9') || s == '-') {
			return fmt.Errorf("name must only contain lowercase letters, digits, or dashes")
		} else if s == '-' {
			if i == 0 {
				return fmt.Errorf("name cannot start with a dash")
			} else if name[i-1] == '-' {
				return fmt.Errorf("name cannot contain repeated dashes")
			}
		}
	}

	return nil
}
