package main

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// toolsSchemaCmd prints the CLI's runnable commands as JSON. A host that offers
// the same actions as typed tools (the daemon bridge) reads this instead of
// scraping --help, so new commands and flags show up without a code change.
var toolsSchemaCmd = &cobra.Command{
	Use:    "tools-schema",
	Short:  "Print the command tree as JSON for tool generators",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(buildToolsSchema(rootCmd))
	},
}

func init() { rootCmd.AddCommand(toolsSchemaCmd) }

type toolsSchema struct {
	Commands []toolsSchemaCommand `json:"commands"`
}

type toolsSchemaCommand struct {
	Path  []string          `json:"path"`
	Short string            `json:"short"`
	Long  string            `json:"long,omitempty"`
	Args  []toolsSchemaArg  `json:"args,omitempty"`
	Flags []toolsSchemaFlag `json:"flags,omitempty"`
}

type toolsSchemaArg struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Variadic bool   `json:"variadic,omitempty"`
}

type toolsSchemaFlag struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Usage   string `json:"usage"`
	Default string `json:"default,omitempty"`
}

func buildToolsSchema(root *cobra.Command) toolsSchema {
	var out toolsSchema
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Deprecated != "" || sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			p := append(append([]string{}, path...), sub.Name())
			if sub.Runnable() {
				out.Commands = append(out.Commands, schemaCommand(sub, p))
			}
			walk(sub, p)
		}
	}
	walk(root, nil)
	return out
}

func schemaCommand(c *cobra.Command, path []string) toolsSchemaCommand {
	sc := toolsSchemaCommand{Path: path, Short: c.Short, Long: strings.TrimSpace(c.Long), Args: parseUseArgs(c.Use)}
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		sc.Flags = append(sc.Flags, toolsSchemaFlag{Name: f.Name, Type: f.Value.Type(), Usage: f.Usage, Default: f.DefValue})
	})
	return sc
}

// parseUseArgs reads positional arguments from a cobra Use line such as
// "assign <id>" or "add [url]...". Tokens in <> are required, tokens in [] are
// optional, a trailing "..." makes the argument repeatable, and [flags] is
// not an argument.
func parseUseArgs(use string) []toolsSchemaArg {
	fields := strings.Fields(use)
	if len(fields) <= 1 {
		return nil
	}
	var args []toolsSchemaArg
	for _, tok := range fields[1:] {
		variadic := strings.HasSuffix(tok, "...")
		tok = strings.TrimSuffix(tok, "...")
		var a toolsSchemaArg
		switch {
		case strings.HasPrefix(tok, "<") && strings.HasSuffix(tok, ">"):
			a = toolsSchemaArg{Name: strings.Trim(tok, "<>"), Required: true}
		case strings.HasPrefix(tok, "[") && strings.HasSuffix(tok, "]"):
			a = toolsSchemaArg{Name: strings.Trim(tok, "[]")}
		default:
			continue
		}
		if a.Name == "flags" || a.Name == "" {
			continue
		}
		a.Variadic = variadic
		args = append(args, a)
	}
	return args
}
