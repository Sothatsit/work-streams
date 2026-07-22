// ws is a thin client for ws-server: a shared, chronological work
// stream of typed entries (notes, todos, decisions, ideas, ...) with
// metadata key=value pairs pointing to other systems (Jira, PRs,
// worklogs, paths).
package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/Sothatsit/work-stream/internal/api"
	"github.com/Sothatsit/work-stream/internal/version"
)

const usage = `ws - a shared work stream of timestamped entries

Usage:
  ws add <type> <subject> [--body <b>] [--meta <key>=<value>]... [shorthands]
  ws recent [search flags]
  ws search [<text>] [search flags]
  ws entry <id>
  ws edit <id> [<subject>] [--subject <s>] [--body <b>]
  ws delete <id>
  ws add-meta <id> <key> <value>
  ws edit-meta <id> <key> <value>
  ws remove-meta <id> <key>
  ws status
  ws secret
  ws --version
  ws help

Entries have a required type (e.g. note, todo, decision, idea), a
short subject, an optional longer body, and any number of metadata
key=value pairs (project, jira, pr, repo, ...). Keep the subject to a
headline and put detail in the body; lists show the subject with a [+]
marker when a body exists, and 'ws entry' shows the body in full.
IDs look like e123. Times are stored in UTC and shown in local time.

Metadata keys are lowercase slugs: letters, digits, and single dashes,
starting with a letter (e.g. jira, github-pr). For several of a kind,
number them: jira-1, jira-2.

Field limits (characters): type 64, subject 128, body 2048, key 64,
value 256. An entry can have 16 metadata pairs.
Type, subject, and metadata values cannot contain control characters.

Shorthands (on add and search) set or match a specific key:
  --project, --jira, --github, --confluence
E.g. ws search --jira 'QUACK-*' is
ws search --meta 'jira=QUACK-*'.

Search flags:
  --subject, --body, --content, --type, --key, and --meta take
  ASCII-case-insensitive GLOB patterns. Patterns match the full value;
  use '*', '?', and bracket classes for wildcards. --content matches
  the subject OR body. Prefix a flag with 'no-' to exclude. --meta
  splits its key=value pattern on the first '='. Shorthands take a
  value pattern. Quote patterns so the shell does not expand them.

  A positional <text> is escaped and matched as a literal substring
  of the subject OR body. All filters AND together, including repeats.

  --limit <n>, -n <n>    max entries to show (1..500, default 50)
  --offset <n>           skip the first n matching entries
  --order-by-creation    newest first by creation time (default)
  --order-by-modified    newest first by modification time
  --id-only              print one entry id per line (for scripting)

'ws recent' is 'ws search' with no filters: the newest 50 entries.

Editing:
  'ws edit' changes subject and/or body; --body "" clears the body.
  Metadata is changed with add-meta/edit-meta/remove-meta. 'ws add-meta'
  refuses to overwrite an existing key; use 'ws edit-meta' to change one.

Server location:
  Global --address, --port, and --timeout flags override
  WORK_STREAM_ADDRESS, WORK_STREAM_PORT, and WORK_STREAM_TIMEOUT.
  Defaults are ` + defaultAddress + `, port ` + defaultPort + `, and 5s.
  WORK_STREAM_SECRET supplies the optional shared bearer secret.
`

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ws: "+err.Error())
	os.Exit(1)
}

func failf(format string, args ...any) {
	fail(fmt.Errorf(format, args...))
}

func extractGlobalFlags(
	args []string,
) (rest []string, address, port, timeout string, err error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := arg, "", false
		if idx := strings.Index(arg, "="); idx >= 0 {
			name = arg[:idx]
			value = arg[idx+1:]
			hasValue = true
		}
		if name != "--address" && name != "--port" && name != "--timeout" {
			rest = append(rest, arg)
			continue
		}
		if seen[name] {
			return nil, "", "", "", fmt.Errorf("%s given more than once", name)
		}
		seen[name] = true
		if !hasValue {
			i++
			if i >= len(args) {
				return nil, "", "", "", fmt.Errorf("%s requires a value", name)
			}
			value = args[i]
		}
		if value == "" {
			return nil, "", "", "", fmt.Errorf("%s requires a value", name)
		}
		if name == "--address" {
			address = value
		} else if name == "--port" {
			port = value
		} else {
			timeout = value
		}
	}
	return rest, address, port, timeout, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return
	}
	args, address, port, timeout, err := extractGlobalFlags(os.Args[1:])
	if err != nil {
		fail(err)
	}
	if len(args) == 0 {
		fmt.Print(usage)
		return
	}
	command := args[0]
	args = args[1:]

	switch command {
	case "help", "--help", "-h":
		fmt.Print(usage)
		return
	case "--version":
		if len(args) != 0 {
			failf("usage: ws --version")
		}
		fmt.Printf("ws %s\n", version.Software)
		return
	case "secret":
		cmdSecret(args)
		return
	}

	c, err := newClient(address, port, timeout)
	if err != nil {
		fail(err)
	}

	switch command {
	case "add":
		cmdAdd(c, args)
	case "recent", "search":
		cmdSearch(c, command, args)
	case "entry":
		cmdEntry(c, args)
	case "edit":
		cmdEdit(c, args)
	case "delete":
		cmdDelete(c, args)
	case "add-meta":
		cmdAddMeta(c, args)
	case "edit-meta":
		cmdEditMeta(c, args)
	case "remove-meta":
		cmdRemoveMeta(c, args)
	case "status":
		cmdStatus(c, args)
	default:
		failf("unknown command %q (run 'ws help')", command)
	}
}

func cmdAdd(c *client, args []string) {
	spec := flagSpec{
		strs:   []string{"--body"},
		repeat: append([]string{"--meta"}, shorthandFlags()...),
	}
	p, err := parseArgs(args, spec)
	if err != nil {
		fail(err)
	}
	if len(p.pos) != 2 {
		failf("usage: ws add <type> <subject> [--body <b>] " +
			"[--meta <key>=<value>]... [--project <p>] [--jira <k>] ...")
	}
	metadata := map[string]string{}
	set := func(key, value string) {
		if _, exists := metadata[key]; exists {
			failf("metadata key %q set more than once; for several use "+
				"--meta %s-1=... --meta %s-2=...", key, key, key)
		}
		metadata[key] = value
	}
	for _, kv := range p.lists["--meta"] {
		key, value, found := strings.Cut(kv, "=")
		if !found {
			failf("invalid --meta %q (expected <key>=<value>)", kv)
		}
		set(key, value)
	}
	for _, key := range shorthandKeys {
		for _, value := range p.lists["--"+key] {
			set(key, value)
		}
	}
	req := api.AddEntryRequest{
		Type:     p.pos[0],
		Subject:  p.pos[1],
		Body:     p.strs["--body"],
		Metadata: metadata,
	}
	if err := req.Validate(); err != nil {
		fail(err)
	}
	entry, err := c.addEntry(req)
	if err != nil {
		fail(err)
	}
	fmt.Printf("Added entry [e%d].\n", entry.ID)
}

func shorthandFlags() []string {
	flags := make([]string, len(shorthandKeys))
	for i, key := range shorthandKeys {
		flags[i] = "--" + key
	}
	return flags
}

func cmdSearch(c *client, command string, args []string) {
	p, err := parseArgs(args, searchFlagSpec())
	if err != nil {
		fail(err)
	}
	if len(p.pos) > 1 {
		failf("at most one positional <text> argument is allowed "+
			"(got %d)", len(p.pos))
	}

	params := url.Values{}
	if len(p.pos) == 1 {
		if command == "recent" {
			failf("'ws recent' takes no <text> argument " +
				"(use 'ws search <text>')")
		}
		params.Add("content", "*"+escapeGlobLiteral(p.pos[0])+"*")
	}
	for flag, values := range p.lists {
		if key, negate, ok := shorthandFor(flag); ok {
			param := "meta"
			if negate {
				param = "no-meta"
			}
			for _, v := range values {
				params.Add(param, key+"="+v)
			}
			continue
		}
		param := strings.TrimPrefix(flag, "--")
		for _, v := range values {
			if (param == "meta" || param == "no-meta") &&
				!strings.Contains(v, "=") {
				failf("%s requires <key>=<value>", flag)
			}
			params.Add(param, v)
		}
	}

	if p.has("--order-by-creation") && p.has("--order-by-modified") {
		failf("--order-by-creation and --order-by-modified " +
			"cannot be combined")
	}
	limit := 50
	if p.has("--limit") {
		if limit, err = parseLimit(p.strs["--limit"]); err != nil {
			fail(err)
		}
	}
	offset := 0
	if p.has("--offset") {
		if offset, err = parseOffset(p.strs["--offset"]); err != nil {
			fail(err)
		}
	}
	params.Set("limit", fmt.Sprint(limit))
	params.Set("offset", fmt.Sprint(offset))
	orderByModified := p.bools["--order-by-modified"]
	if orderByModified {
		params.Set("order-by", "modified")
	}

	result, err := c.search(params)
	if err != nil {
		fail(err)
	}
	printSearchResult(result, offset, limit, orderByModified,
		p.bools["--id-only"])
}

func escapeGlobLiteral(value string) string {
	var escaped strings.Builder
	for _, r := range value {
		switch r {
		case '*':
			escaped.WriteString("[*]")
		case '?':
			escaped.WriteString("[?]")
		case '[':
			escaped.WriteString("[[]")
		default:
			escaped.WriteRune(r)
		}
	}
	return escaped.String()
}

func cmdEntry(c *client, args []string) {
	if len(args) != 1 {
		failf("usage: ws entry <id>")
	}
	id, err := parseEntryID(args[0])
	if err != nil {
		fail(err)
	}
	entry, err := c.getEntry(id)
	if err != nil {
		fail(err)
	}
	printEntryDetail(entry)
}

func cmdEdit(c *client, args []string) {
	spec := flagSpec{strs: []string{"--subject", "--body"}}
	p, err := parseArgs(args, spec)
	if err != nil {
		fail(err)
	}
	if len(p.pos) < 1 || len(p.pos) > 2 {
		failf("usage: ws edit <id> [<subject>] [--subject <s>] [--body <b>]")
	}
	id, err := parseEntryID(p.pos[0])
	if err != nil {
		fail(err)
	}
	req := api.EditEntryRequest{}
	if len(p.pos) == 2 {
		if p.has("--subject") {
			failf("positional <subject> and --subject are both given; " +
				"use one or the other")
		}
		req.Subject = &p.pos[1]
	} else if p.has("--subject") {
		subject := p.strs["--subject"]
		req.Subject = &subject
	}
	if p.has("--body") {
		body := p.strs["--body"]
		req.Body = &body
	}
	if err := req.Validate(); err != nil {
		fail(err)
	}
	entry, err := c.editEntry(id, req)
	if err != nil {
		fail(err)
	}
	fmt.Printf("Edited entry [e%d].\n", entry.ID)
}

func cmdDelete(c *client, args []string) {
	if len(args) != 1 {
		failf("usage: ws delete <id>")
	}
	id, err := parseEntryID(args[0])
	if err != nil {
		fail(err)
	}
	if err := c.deleteEntry(id); err != nil {
		fail(err)
	}
	fmt.Printf("Deleted entry [e%d].\n", id)
}

func metaArgs(args []string, usage string) (int64, string, string) {
	if len(args) != 3 {
		failf("usage: %s", usage)
	}
	id, err := parseEntryID(args[0])
	if err != nil {
		fail(err)
	}
	return id, args[1], args[2]
}

func cmdAddMeta(c *client, args []string) {
	id, key, value := metaArgs(args, "ws add-meta <id> <key> <value>")
	entry, err := c.addMeta(id, key, value)
	if err != nil {
		fail(err)
	}
	fmt.Printf("Added metadata %s to entry [e%d].\n", key, entry.ID)
}

func cmdEditMeta(c *client, args []string) {
	id, key, value := metaArgs(args, "ws edit-meta <id> <key> <value>")
	entry, err := c.editMeta(id, key, value)
	if err != nil {
		fail(err)
	}
	fmt.Printf("Edited metadata %s on entry [e%d].\n", key, entry.ID)
}

func cmdRemoveMeta(c *client, args []string) {
	if len(args) != 2 {
		failf("usage: ws remove-meta <id> <key>")
	}
	id, err := parseEntryID(args[0])
	if err != nil {
		fail(err)
	}
	entry, err := c.removeMeta(id, args[1])
	if err != nil {
		fail(err)
	}
	fmt.Printf("Removed metadata %s from entry [e%d].\n", args[1], entry.ID)
}

func cmdStatus(c *client, args []string) {
	if len(args) != 0 {
		failf("usage: ws status")
	}
	status, err := c.status()
	if err != nil {
		var responseErr *serverResponseError
		if errors.As(err, &responseErr) &&
			responseErr.StatusCode == http.StatusUnauthorized {
			fmt.Println("address: " + c.baseURL)
			fmt.Println("connectivity: reachable")
			if c.secret == "" {
				fmt.Println("secret: required; WORK_STREAM_SECRET is not set")
			} else {
				fmt.Println("secret: WORK_STREAM_SECRET was rejected")
			}
			failf("authentication failed")
		}
		if errors.As(err, &responseErr) && c.secret != "" &&
			responseErr.Message == "server authentication is disabled" {
			fmt.Println("address: " + c.baseURL)
			fmt.Println("connectivity: reachable")
			fmt.Println("secret: disabled; WORK_STREAM_SECRET is set locally")
			failf("server authentication is disabled")
		}
		fail(err)
	}

	fmt.Println("address: " + c.baseURL)
	fmt.Println("connectivity: ok")
	fmt.Println("server: " + status.Version)
	fmt.Println("database: " + status.Database)
	fmt.Println("data: " + status.Data)
	fmt.Printf("timeout: client %s, server %s\n", c.timeout, status.Timeout)
	if status.Authentication {
		fmt.Println("secret: enabled; local value accepted")
		return
	}
	if c.secret != "" {
		fmt.Println("secret: disabled; WORK_STREAM_SECRET is set locally")
		failf("server authentication is disabled")
	}
	fmt.Println("secret: disabled")
	fmt.Println("hint: run 'ws secret' and set WORK_STREAM_SECRET on " +
		"the server and clients")
}

func cmdSecret(args []string) {
	if len(args) != 0 {
		failf("usage: ws secret")
	}
	secret := os.Getenv(secretEnvironment)
	if secret != "" {
		if err := validateSecret(secret); err != nil {
			fail(err)
		}
		fmt.Println("WORK_STREAM_SECRET is already set.")
		return
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		fail(fmt.Errorf("generating secret: %w", err))
	}
	fmt.Println(base64.RawURLEncoding.EncodeToString(secretBytes))
}
