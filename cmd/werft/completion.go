package main

import (
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/hk9890/werft/internal/failure"
)

func (c *cli) cmdCompletion(args []string) error {
	fs := flag.NewFlagSet("completion", flag.ContinueOnError)
	if err := c.parseFlags(fs, "completion", args); err != nil {
		return err
	}
	if fs.NArg() != 1 || fs.Arg(0) != "zsh" {
		return failure.New(failure.Usage, "completion takes one shell: zsh")
	}
	script, err := zshCompletion()
	if err != nil {
		return err
	}
	c.output(map[string]string{"shell": "zsh", "script": script}, func(w io.Writer) { _, _ = fmt.Fprint(w, script) })
	return nil
}

// completers maps an argument placeholder of the command table to the zsh
// action that completes it.
var completers = map[string]string{
	"name":     "_werft_domains",
	"build-vm": "_werft_domains",
	"label":    "_werft_labels",
	"image":    "_werft_images",
	"lease-id": "_werft_leases",
	"command":  "_werft_commands",
	"shell":    "(zsh)",
}

// argToken is one bracket group of command.args: "<name>", "[label]",
// "[--tier plain|full]".
var argToken = regexp.MustCompile(`<[^>]*>|\[[^\]]*\]`)

// zshArguments returns the _arguments specs of cmd, read from its args.
// When no flag follows a positional argument, the flags must come first, as
// the flag package parses them.
func zshArguments(cmd command) ([]string, error) {
	var specs []string
	position := 0
	flagsFirst := true
	for _, token := range argToken.FindAllString(cmd.args, -1) {
		optional := token[0] == '['
		inner := token[1 : len(token)-1]
		if strings.HasPrefix(inner, "--") {
			if position > 0 {
				flagsFirst = false
			}
			specs = append(specs, zshFlag(inner))
			continue
		}
		action, ok := completers[inner]
		if !ok {
			return nil, failure.New(failure.Failed, "%s: no completion for argument %s", cmd.name, token)
		}
		position++
		colons := ":"
		if optional {
			colons = "::"
		}
		specs = append(specs, fmt.Sprintf("%d%s%s:%s", position, colons, inner, action))
	}
	if flagsFirst && position > 0 && len(specs) > position {
		specs = append([]string{"-A", "-*"}, specs...)
	}
	return specs, nil
}

// zshFlag turns "--yes", "--tier plain|full" or "--recipe file" into an
// _arguments spec.
func zshFlag(flagArgs string) string {
	name, value, hasValue := strings.Cut(flagArgs, " ")
	if !hasValue {
		return name
	}
	action := " "
	switch {
	case strings.Contains(value, "|"):
		action = "(" + strings.ReplaceAll(value, "|", " ") + ")"
	case value == "file":
		action = "_files"
	case value == "mount":
		action = "_directories"
	}
	return fmt.Sprintf("%s=:%s:%s", name, strings.TrimPrefix(name, "--"), action)
}

func zshQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func zshCompletion() (string, error) {
	var b strings.Builder
	b.WriteString(zshHead)
	b.WriteString("\n_werft_commands() {\n  local -a commands=(\n")
	for _, cmd := range commands {
		fmt.Fprintf(&b, "    %s\n", zshQuote(cmd.name+":"+cmd.summary))
	}
	b.WriteString("  )\n  _describe -t commands 'werft command' commands\n}\n")
	b.WriteString("\n_werft() {\n")
	b.WriteString("  local curcontext=$curcontext state line _werft_prog=$words[1]\n")
	b.WriteString("  _arguments -C '--json[print one JSON document on stdout]' '1:command:->command' '*::argument:->argument'\n")
	b.WriteString("  case $state in\n")
	b.WriteString("    command) _werft_commands ;;\n")
	b.WriteString("    argument)\n")
	b.WriteString("      curcontext=${curcontext%:*:*}:werft-$words[1]:\n")
	b.WriteString("      case $words[1] in\n")
	for _, cmd := range commands {
		specs, err := zshArguments(cmd)
		if err != nil {
			return "", err
		}
		if len(specs) == 0 {
			continue
		}
		quoted := make([]string, len(specs))
		for i, spec := range specs {
			quoted[i] = zshQuote(spec)
		}
		fmt.Fprintf(&b, "        %s) _arguments %s ;;\n", cmd.name, strings.Join(quoted, " "))
	}
	b.WriteString("      esac ;;\n  esac\n}\n")
	b.WriteString(zshTail)
	return b.String(), nil
}

// zshHead holds the helpers that ask werft for names. They read the indented
// JSON of werft --json line by line, so the completion needs no jq.
const zshHead = `#compdef werft
# zsh completion for werft; source <(werft completion zsh)

# _werft_values key command...: the string values of key in the JSON of
# werft --json command.
_werft_values() {
  local key=$1 l
  shift
  reply=()
  for l in ${(f)"$(${_werft_prog:-werft} --json "$@" 2>/dev/null)"}; do
    [[ $l == *\"$key\":\ \"*\"* ]] && reply+=(${${l#*\": \"}%\"*})
  done
}

_werft_domains() {
  local expl
  _werft_values name list
  _wanted domains expl 'VM' compadd -a reply
}

_werft_images() {
  local expl
  _werft_values image images
  _wanted images expl 'stock image' compadd -a reply
}

# The lease list releases expired leases first, as werft leases does.
_werft_leases() {
  local expl
  _werft_values id leases
  _wanted leases expl 'lease' compadd -a reply
}

# _werft_labels: the reset points of the VM in the first argument.
_werft_labels() {
  local expl l inside=0
  local -a labels
  [[ -n $line[1] ]] || return 1
  for l in ${(f)"$(${_werft_prog:-werft} --json snapshots $line[1] 2>/dev/null)"}; do
    case $l in
      *'"reset_points": []'*) break ;;
      *'"reset_points": ['*) inside=1 ;;
      *']'*) inside=0 ;;
      *\"*) (( inside )) && labels+=(${${l#*\"}%\"*}) ;;
    esac
  done
  _wanted labels expl 'reset point' compadd -a labels
}
`

const zshTail = `
if [[ $zsh_eval_context[-1] == loadautofunc ]]; then
  _werft "$@"
else
  compdef _werft werft
fi
`
