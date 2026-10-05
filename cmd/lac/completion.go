package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sadeq.uk/lac/pkg/lacclient"
)

// completeCommandName is the hidden command the shell scripts call on every Tab. What can be
// completed is decided here, once, so the bash, zsh and fish scripts are thin shims that cannot
// drift apart.
const completeCommandName = "__complete"

// completionLookupTimeout bounds a Tab press that has to ask the daemon. A completion that hangs
// is worse than one that offers nothing.
const completionLookupTimeout = time.Second

// valueSource is where the candidates for an argument come from.
type valueSource int

const (
	// sourceNone offers nothing, leaving the shell to fall back to file names.
	sourceNone valueSource = iota
	sourceResources
	sourceAgents
	sourceLeases
)

// completionValues describes what may be typed into one argument or flag value.
type completionValues struct {
	source  valueSource
	choices []string
}

// completionFlag is one flag a command accepts.
type completionFlag struct {
	name        string
	description string
	takesValue  bool
	values      completionValues
}

// completionSpec is what one command accepts, in the order its positional arguments come.
type completionSpec struct {
	flags      []completionFlag
	positional []completionValues
}

func booleanFlag(name, description string) completionFlag {
	return completionFlag{name: name, description: description}
}

func valueFlag(name, description string) completionFlag {
	return completionFlag{name: name, description: description, takesValue: true}
}

func sourcedFlag(name, description string, source valueSource) completionFlag {
	return completionFlag{
		name: name, description: description, takesValue: true, values: completionValues{source: source},
	}
}

func fromSource(source valueSource) completionValues { return completionValues{source: source} }

func fromChoices(choices ...string) completionValues { return completionValues{choices: choices} }

// globalCompletionFlags mirror the flags run() parses before the command name.
var globalCompletionFlags = []completionFlag{
	booleanFlag("json", "print machine-readable JSON instead of a table"),
	valueFlag("kind", "the tool behind this agent"),
	sourcedFlag("name", "register under this name", sourceAgents),
	valueFlag("socket", "the daemon's socket"),
	valueFlag("token", "authenticate with this token instead of registering"),
	valueFlag("workdir", "the directory being worked in"),
}

// completionSpecs mirror the flag sets each command parses. A command missing from here is caught
// by a test, so a new command cannot quietly go without completion.
var completionSpecs = map[string]completionSpec{
	"version":    {},
	"info":       {},
	"register":   {flags: []completionFlag{booleanFlag("save", "save the token for later commands")}},
	"agents":     {},
	"resources":  {},
	"reload":     {},
	"held":       {},
	"commands":   {},
	"mcp":        {},
	"deregister": {},
	"send": {
		flags:      []completionFlag{valueFlag("topic", "send to a topic instead of an agent")},
		positional: []completionValues{fromSource(sourceAgents), fromChoices("status", "question", "answer")},
	},
	"inbox": {flags: []completionFlag{
		booleanFlag("ack", "acknowledge what is read"),
		valueFlag("limit", "how many messages to show"),
	}},
	"log": {flags: []completionFlag{
		sourcedFlag("agent", "only what this agent sent or was sent", sourceAgents),
		booleanFlag("follow", "keep watching"),
		valueFlag("limit", "how many messages to show"),
		valueFlag("since", "only messages since this time, such as 1h"),
		valueFlag("topic", "only messages published to this topic"),
	}},
	"define": {
		flags: []completionFlag{
			valueFlag("capacity", "how many agents may hold a slot at once"),
			valueFlag("description", "what the resource is for"),
			valueFlag("ttl", "how long a slot lasts unless renewed"),
		},
		positional: []completionValues{fromSource(sourceResources)},
	},
	"queue": {positional: []completionValues{fromSource(sourceResources)}},
	"acquire": {
		flags: []completionFlag{
			booleanFlag("no-wait", "fail immediately if the resource is full"),
			valueFlag("priority", "higher goes first"),
			valueFlag("reason", "what the slot is for, shown in the queue"),
			valueFlag("timeout", "give up after this long"),
		},
		positional: []completionValues{fromSource(sourceResources)},
	},
	"release": {
		flags:      []completionFlag{booleanFlag("force", "take the slot back from whichever agent holds it")},
		positional: []completionValues{fromSource(sourceLeases)},
	},
	"run": {flags: []completionFlag{
		valueFlag("priority", "higher goes first"),
		booleanFlag("quiet", "say nothing about the slot"),
		valueFlag("reason", "what the slot is for, shown in the queue"),
		sourcedFlag("resource", "the resource to queue for", sourceResources),
		valueFlag("timeout", "give up after this long"),
	}},
	"ask": {flags: []completionFlag{
		booleanFlag("background", "do not wait for the result"),
		valueFlag("timeout", "give up after this long"),
		valueFlag("workdir", "run it in this directory"),
	}},
	"task":  {flags: []completionFlag{booleanFlag("wait", "wait for it to finish")}},
	"tasks": {flags: []completionFlag{valueFlag("limit", "how many tasks to show")}},
	"worker": {flags: []completionFlag{
		booleanFlag("once", "run one job and stop"),
		booleanFlag("quiet", "say less"),
		sourcedFlag("resource", "the resource to work for", sourceResources),
	}},
	"skill": {
		flags: []completionFlag{
			valueFlag("dir", "where to install"),
			booleanFlag("print", "write the skill to stdout instead of installing it"),
		},
		positional: []completionValues{fromChoices("install", "print", "path")},
	},
	"pr":     {positional: []completionValues{fromChoices("watch", "status", "list", "unwatch")}},
	"report": {flags: []completionFlag{valueFlag("deadline", "how long to wait for answers")}},
	"answer": {},
	"completion": {
		flags:      []completionFlag{booleanFlag("install", "write the script where the shell loads it")},
		positional: []completionValues{fromChoices("bash", "zsh", "fish")},
	},
}

// candidate is one completion: the text to insert and a description some shells show beside it.
type candidate struct {
	value       string
	description string
}

// completionLookup fetches the candidates that only the daemon knows.
type completionLookup interface {
	lookup(ctx context.Context, source valueSource) []candidate
}

// complete returns the candidates for the last word, given every word after "lac" up to it.
func complete(ctx context.Context, lookup completionLookup, words []string) []candidate {
	if len(words) == 0 {
		words = []string{""}
	}
	current := words[len(words)-1]
	previous := words[:len(words)-1]

	// Skip the global flags to find the command.
	index := 0
	for index < len(previous) && strings.HasPrefix(previous[index], "-") {
		if found := findFlag(globalCompletionFlags, previous[index]); found != nil && found.takesValue &&
			!strings.Contains(previous[index], "=") {
			index++
		}
		index++
	}

	if index > len(previous) {
		// The current word is the value of the last global flag.
		return valuesFor(ctx, lookup, findFlag(globalCompletionFlags, previous[len(previous)-1]).values, current)
	}
	if index == len(previous) {
		if strings.HasPrefix(current, "-") {
			return flagCandidates(globalCompletionFlags, current)
		}
		return commandCandidates(current)
	}

	spec, known := completionSpecs[previous[index]]
	if !known {
		return nil
	}

	position := 0
	var expectingValue *completionFlag
	for _, word := range previous[index+1:] {
		switch {
		case expectingValue != nil:
			expectingValue = nil
		case word == "--":
			// Everything after this belongs to another program, such as the command `lac run` runs.
			return nil
		case strings.HasPrefix(word, "-") && len(word) > 1:
			if found := findFlag(spec.flags, word); found != nil && found.takesValue && !strings.Contains(word, "=") {
				expectingValue = found
			}
		default:
			position++
		}
	}

	if expectingValue != nil {
		return valuesFor(ctx, lookup, expectingValue.values, current)
	}
	if strings.HasPrefix(current, "-") {
		return flagCandidates(spec.flags, current)
	}
	if position < len(spec.positional) {
		return valuesFor(ctx, lookup, spec.positional[position], current)
	}

	return nil
}

func findFlag(flags []completionFlag, word string) *completionFlag {
	name := strings.TrimLeft(word, "-")
	name, _, _ = strings.Cut(name, "=")
	for index := range flags {
		if flags[index].name == name {
			return &flags[index]
		}
	}

	return nil
}

func commandCandidates(prefix string) []candidate {
	var found []candidate
	for _, available := range commands() {
		if !available.hidden && strings.HasPrefix(available.name, prefix) {
			found = append(found, candidate{value: available.name, description: available.summary})
		}
	}

	return found
}

func flagCandidates(flags []completionFlag, prefix string) []candidate {
	var found []candidate
	for _, available := range flags {
		value := "--" + available.name
		if strings.HasPrefix(value, prefix) || strings.HasPrefix("-"+available.name, prefix) {
			found = append(found, candidate{value: value, description: available.description})
		}
	}

	return found
}

func valuesFor(ctx context.Context, lookup completionLookup, values completionValues, prefix string) []candidate {
	var found []candidate
	for _, choice := range values.choices {
		if strings.HasPrefix(choice, prefix) {
			found = append(found, candidate{value: choice})
		}
	}

	if values.source != sourceNone {
		for _, offered := range lookup.lookup(ctx, values.source) {
			if strings.HasPrefix(offered.value, prefix) {
				found = append(found, offered)
			}
		}
	}

	return found
}

// daemonLookup asks the running daemon, connecting once and only if a Tab press needs it.
type daemonLookup struct {
	env    *environment
	client *lacclient.Client
	done   func()
	failed bool
}

func (d *daemonLookup) connect(ctx context.Context) *lacclient.Client {
	if d.client == nil && !d.failed {
		client, release, _, err := d.env.identity.connect(ctx, d.env.socketPath)
		if err != nil {
			d.failed = true
			return nil
		}
		d.client, d.done = client, release
	}

	return d.client
}

func (d *daemonLookup) close() {
	if d.done != nil {
		d.done()
	}
}

// lookup returns nothing on any error: a daemon that is down must not put errors in the prompt.
func (d *daemonLookup) lookup(ctx context.Context, source valueSource) []candidate {
	client := d.connect(ctx)
	if client == nil {
		return nil
	}

	var found []candidate
	switch source {
	case sourceResources:
		resources, err := client.Resources(ctx)
		if err != nil {
			return nil
		}
		for _, resource := range resources {
			found = append(found, candidate{value: resource.Name, description: resource.Description})
		}

	case sourceAgents:
		agents, err := client.Agents(ctx)
		if err != nil {
			return nil
		}
		for _, agent := range agents {
			found = append(found, candidate{value: agent.Name, description: agent.Workdir})
		}

	case sourceLeases:
		resources, err := client.Resources(ctx)
		if err != nil {
			return nil
		}
		for _, resource := range resources {
			if resource.Held == 0 {
				continue
			}
			status, err := client.Queue(ctx, resource.Name)
			if err != nil {
				continue
			}
			for _, holder := range status.Holders {
				found = append(found, candidate{
					value:       holder.ID,
					description: fmt.Sprintf("%s held by %s", holder.Resource, holder.AgentName),
				})
			}
		}
	}

	return found
}

// runComplete prints one candidate per line as "value<TAB>description" for the shell scripts.
func runComplete(ctx context.Context, env *environment, arguments []string) error {
	ctx, cancel := context.WithTimeout(ctx, completionLookupTimeout)
	defer cancel()

	lookup := &daemonLookup{env: env}
	defer lookup.close()

	writeCandidates(os.Stdout, complete(ctx, lookup, arguments))

	return nil
}

func writeCandidates(writer io.Writer, candidates []candidate) {
	for _, offered := range candidates {
		description := strings.ReplaceAll(offered.description, "\n", " ")
		fmt.Fprintf(writer, "%s\t%s\n", offered.value, description)
	}
}

// runCompletion prints, or installs, the completion script for a shell.
func runCompletion(_ context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("completion", flag.ContinueOnError)
	install := flags.Bool("install", false, "write the script where the shell loads it instead of printing it")
	if err := parseAnywhere(flags, arguments); err != nil {
		return err
	}

	script, known := completionScripts[flags.Arg(0)]
	if flags.NArg() != 1 || !known {
		return errors.New("usage: lac completion [--install] <bash|zsh|fish>")
	}

	if !*install {
		_, err := fmt.Fprint(os.Stdout, script)
		return err //nolint:wrapcheck // a failed write to stdout needs no more context
	}

	paths, err := installCompletion(flags.Arg(0), script)
	if err != nil {
		return err
	}
	for _, path := range paths {
		fmt.Fprintf(env.output, "installed\t%s\n", path)
	}
	if hint := completionHints[flags.Arg(0)]; hint != "" {
		_ = env.output.Flush()
		fmt.Fprintln(os.Stderr, hint)
	}

	return nil
}

// completionTargets are where each shell looks for completions without any change to its startup
// files, relative to the XDG data or configuration directory.
func completionTargets(shell string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("finding the home directory: %w", err)
	}

	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}

	switch shell {
	case "bash":
		// bash-completion loads a file named after the command, so lacd needs its own copy.
		directory := filepath.Join(dataHome, "bash-completion", "completions")
		return []string{filepath.Join(directory, "lac"), filepath.Join(directory, "lacd")}, nil
	case "zsh":
		// One file serves both: its #compdef line names lac and lacd.
		return []string{filepath.Join(dataHome, "zsh", "site-functions", "_lac")}, nil
	case "fish":
		directory := filepath.Join(configHome, "fish", "completions")
		return []string{filepath.Join(directory, "lac.fish"), filepath.Join(directory, "lacd.fish")}, nil
	}

	return nil, fmt.Errorf("no completion for the shell %q", shell)
}

func installCompletion(shell, script string) ([]string, error) {
	targets, err := completionTargets(shell)
	if err != nil {
		return nil, err
	}

	for _, target := range targets {
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
		}
		if err := os.WriteFile(target, []byte(script), 0o600); err != nil {
			return nil, fmt.Errorf("writing %s: %w", target, err)
		}
	}

	return targets, nil
}

// completionHints say what the shell needs before it picks the new file up.
var completionHints = map[string]string{
	"bash": "Needs the bash-completion package; open a new shell to load it.",
	"zsh": "The directory must be in fpath before compinit runs. Open a new shell; if it is not picked up, " +
		"remove ~/.zcompdump* and start again.",
	"fish": "Open a new shell to load it.",
}

var completionScripts = map[string]string{
	"bash": bashCompletionScript,
	"zsh":  zshCompletionScript,
	"fish": fishCompletionScript,
}

const bashCompletionScript = `# bash completion for lac and lacd. Generated by: lac completion bash
_lac() {
    local line
    COMPREPLY=()
    while IFS= read -r line; do
        COMPREPLY+=("${line%%$'\t'*}")
    done < <(command lac __complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null)
}
complete -o default -F _lac lac

_lacd() {
    local current=${COMP_WORDS[COMP_CWORD]}
    case ${COMP_WORDS[COMP_CWORD-1]} in
        -log-level|--log-level) COMPREPLY=($(compgen -W "debug info warn error" -- "$current")); return ;;
        -config|--config|-database|--database|-socket|--socket) COMPREPLY=(); return ;;
    esac
    COMPREPLY=($(compgen -W "-config -database -log-level -socket -version" -- "$current"))
}
complete -o default -F _lacd lacd
`

const zshCompletionScript = `#compdef lac lacd
# zsh completion for lac and lacd. Generated by: lac completion zsh

_lacd_arguments() {
    _arguments \
        '-config[configuration file]:file:_files' \
        '-database[database file]:file:_files' \
        '-log-level[log level]:level:(debug info warn error)' \
        '-socket[socket to listen on]:socket:_files' \
        '-version[print the version and exit]'
}

_lac() {
    if [[ $service == lacd ]]; then
        _lacd_arguments
        return
    fi

    local -a candidates
    local line value
    for line in ${(f)"$(command lac __complete "${(@)words[2,CURRENT]}" 2>/dev/null)"}; do
        value=${line%%$'\t'*}
        candidates+=("${value//:/\\:}:${line#*$'\t'}")
    done

    if (( ${#candidates} )); then
        _describe -t values 'lac' candidates
    else
        _files
    fi
}

if [[ $funcstack[1] == _lac ]]; then
    _lac "$@"
else
    compdef _lac lac lacd
fi
`

const fishCompletionScript = `# fish completion for lac and lacd. Generated by: lac completion fish
function __lac_complete
    set -l words (commandline -opc)
    set -e words[1]
    command lac __complete $words (commandline -ct) 2>/dev/null
end
complete -c lac -f -a '(__lac_complete)'

complete -c lacd -o config -r -F -d 'configuration file'
complete -c lacd -o database -r -F -d 'database file'
complete -c lacd -o log-level -x -a 'debug info warn error' -d 'log level'
complete -c lacd -o socket -r -F -d 'socket to listen on'
complete -c lacd -o version -d 'print the version and exit'
`
