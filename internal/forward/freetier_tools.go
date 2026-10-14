// Verbatim tool definitions captured 2026-10-14 from the official OpenCode
// v1.19.0 client (real CLI traffic replayed through a local collector). The
// free-tier gate validates these two definitions -- description AND parameter
// schema -- essentially byte-for-byte; minimal stubbed synonyms are refused
// with FreeTierError. Keep in sync with docs/FREE-TIER.md.
package forward

import "encoding/json"

// gateToolBashJSON is the bash tool exactly as the official client sells it.
const gateToolBashJSON = "" +
	"{\"type\":\"function\",\"name\":\"bash\",\"description\":\"Executes a given bash command in a pe" +
	"rsistent shell session with optional timeout, ensuring proper handling and security measures.\\n" +
	"\\nBe aware: OS: linux, Shell: bash\\n\\nAll commands run in the current working directory by de" +
	"fault. Use the `workdir` parameter if you need to run a command in a different directory. AVOID " +
	"using `cd <directory> && <command>` patterns - use `workdir` instead.\\n\\nUse `/tmp/opencode` f" +
	"or temporary work outside the workspace. This directory has already been created, already exists" +
	", and is pre-approved for external directory access.\\n\\nIMPORTANT: This tool is for terminal o" +
	"perations like git, npm, docker, etc. DO NOT use it for file operations (reading, writing, editi" +
	"ng, searching, finding files) - use the specialized tools for this instead.\\n\\nBefore executin" +
	"g the command, please follow these steps:\\n\\n1. Directory Verification:\\n   - If the command " +
	"will create new directories or files, first use `ls` to verify the parent directory exists and i" +
	"s the correct location\\n   - For example, before running \\\"mkdir foo/bar\\\", first use `ls f" +
	"oo` to check that \\\"foo\\\" exists and is the intended parent directory\\n\\n2. Command Execut" +
	"ion:\\n   - Always quote file paths that contain spaces with double quotes (e.g., rm \\\"path wi" +
	"th spaces/file.txt\\\")\\n   - Examples of proper quoting:\\n     - mkdir \\\"/Users/name/My Doc" +
	"uments\\\" (correct)\\n     - mkdir /Users/name/My Documents (incorrect - will fail)\\n     - py" +
	"thon \\\"/path/with spaces/script.py\\\" (correct)\\n     - python /path/with spaces/script.py (" +
	"incorrect - will fail)\\n   - After ensuring proper quoting, execute the command.\\n   - Capture" +
	" the output of the command.\\n\\nUsage notes:\\n  - The command argument is required.\\n  - You " +
	"can specify an optional timeout in milliseconds. If not specified, commands will time out after " +
	"120000ms.\\n  - If the output exceeds 2000 lines or 51200 bytes, it will be truncated and the fu" +
	"ll output will be written to a file. You can use Read with offset/limit to read specific section" +
	"s or Grep to search the full content. Do NOT use `head`, `tail`, or other truncation commands to" +
	" limit output; the full output will already be captured to a file for more precise searching.\\n" +
	"\\n  - Avoid using Bash with the `find`, `grep`, `cat`, `head`, `tail`, `sed`, `awk`, or `echo` " +
	"commands, unless explicitly instructed or when these commands are truly necessary for the task. " +
	"Instead, always prefer using the dedicated tools for these commands:\\n    - File search: Use Gl" +
	"ob (NOT find or ls)\\n    - Content search: Use Grep (NOT grep or rg)\\n    - Read files: Use Re" +
	"ad (NOT cat/head/tail)\\n    - Edit files: Use Edit (NOT sed/awk)\\n    - Write files: Use Write" +
	" (NOT echo >/cat <<EOF)\\n    - Communication: Output text directly (NOT echo/printf)\\n  - When" +
	" issuing multiple commands:\\n    - If the commands are independent and can run in parallel, mak" +
	"e multiple bash tool calls in a single message. For example, if you need to run \\\"git status" +
	"\\\" and \\\"git diff\\\", send a single message with two bash tool calls in parallel.\\n    - I" +
	"f the commands depend on each other and must run sequentially, use a single Bash call with '&&' " +
	"to chain them together (e.g., `git add . && git commit -m \\\"message\\\" && git push`). For ins" +
	"tance, if one operation must complete before another starts (like mkdir before cp, Write before " +
	"Bash for git operations, or git add before git commit), run these operations sequentially instea" +
	"d.\\n    - Use ';' only when you need to run commands sequentially but don't care if earlier com" +
	"mands fail\\n    - DO NOT use newlines to separate commands (newlines are ok in quoted strings)" +
	"\\n  - AVOID using `cd <directory> && <command>`. Use the `workdir` parameter to change director" +
	"ies instead.\\n    <good-example>\\n    Use workdir=\\\"/foo/bar\\\" with command: pytest tests" +
	"\\n    </good-example>\\n    <bad-example>\\n    cd /foo/bar && pytest tests\\n    </bad-example" +
	">\\n\\n# Git and GitHub\\n- Only commit, amend, push, or create PRs when explicitly requested." +
	"\\n- Before committing, inspect `git status`, `git diff`, and `git log --oneline -10`; stage onl" +
	"y intended files and never commit secrets.\\n- Write a concise commit message that matches the r" +
	"epo style.\\n- Do not update git config, skip hooks, use interactive `-i`, force-push, or create" +
	" empty commits unless explicitly requested.\\n- If a commit fails or hooks reject it, fix the is" +
	"sue and create a new commit; do not amend the failed commit.\\n- Before creating a PR, inspect s" +
	"tatus, diff, remote tracking, recent commits, and the diff from the base branch.\\n- Review all " +
	"commits included in the PR, not just the latest commit.\\n- Use `gh` for GitHub tasks, including" +
	" PRs, issues, checks, and releases; return the PR URL when done.\\n\",\"parameters\":{\"properti" +
	"es\":{\"command\":{\"description\":\"The command to execute\",\"type\":\"string\"},\"timeout\":{" +
	"\"description\":\"Optional timeout in milliseconds\",\"type\":\"integer\"},\"workdir\":{\"descri" +
	"ption\":\"The working directory to run the command in. Defaults to the current directory. Use th" +
	"is instead of 'cd' commands.\",\"type\":\"string\"}},\"required\":[\"command\"],\"type\":\"objec" +
	"t\"},\"strict\":false}"

// gateToolReadJSON is the read tool exactly as the official client sells it.
const gateToolReadJSON = "" +
	"{\"type\":\"function\",\"name\":\"read\",\"description\":\"Read a file or directory from the loc" +
	"al filesystem. If the path does not exist, an error is returned.\\n\\nUsage:\\n- The filePath pa" +
	"rameter should be an absolute path.\\n- By default, this tool returns up to 2000 lines from the " +
	"start of the file.\\n- The offset parameter is the line number to start from (1-indexed).\\n- To" +
	" read later sections, call this tool again with a larger offset.\\n- Use the grep tool to find s" +
	"pecific content in large files or files with long lines.\\n- If you are unsure of the correct fi" +
	"le path, use the glob tool to look up filenames by glob pattern.\\n- Contents are returned with " +
	"each line prefixed by its line number as `<line>: <content>`. For example, if a file has content" +
	"s \\\"foo\\\\n\\\", you will receive \\\"1: foo\\\\n\\\". For directories, entries are returned " +
	"one per line (without line numbers) with a trailing `/` for subdirectories.\\n- Any line longer " +
	"than 2000 characters is truncated.\\n- Call this tool in parallel when you know there are multip" +
	"le files you want to read.\\n- Avoid tiny repeated slices (30 line chunks). If you need more con" +
	"text, read a larger window.\\n- This tool can read image files and PDFs and return them as file " +
	"attachments.\\n\",\"parameters\":{\"properties\":{\"filePath\":{\"description\":\"The absolute p" +
	"ath to the file or directory to read\",\"type\":\"string\"},\"offset\":{\"description\":\"The li" +
	"ne number to start reading from (1-indexed)\",\"type\":\"integer\"},\"limit\":{\"description\":" +
	"\"The maximum number of lines to read (defaults to 2000)\",\"type\":\"integer\"}},\"required\":[" +
	"\"filePath\"],\"type\":\"object\"},\"strict\":false}"

// gateToolBody mirrors the checked content of one gate tool definition in the
// flat form the official client stamps on the Responses surface. Field order
// follows the capture so the emitted bytes stay stable.
type gateToolBody struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Parameters  json.RawMessage  `json:"parameters"`
	Strict      *json.RawMessage `json:"strict,omitempty"`
}

// gateToolDef is one parsed verbatim definition with its flat/nested
// renderings memoized at init.
type gateToolDef struct {
	flat   json.RawMessage
	nested json.RawMessage
}

func (d *gateToolDef) render(shape toolShape) (json.RawMessage, error) {
	if shape == toolShapeFlat {
		return d.flat, nil
	}
	return d.nested, nil
}

var gateToolDefs map[string]*gateToolDef

func init() {
	parse := func(constJSON string) gateToolBody {
		var b gateToolBody
		if err := json.Unmarshal([]byte(constJSON), &b); err != nil {
			panic("forward: captured gate tool definition does not parse: " + err.Error())
		}
		return b
	}
	render := func(b gateToolBody) (*gateToolDef, error) {
		flat, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		nested, err := json.Marshal(struct {
			Type     string       `json:"type"`
			Function gateToolBody `json:"function"`
		}{Type: "function", Function: b})
		if err != nil {
			return nil, err
		}
		return &gateToolDef{flat: flat, nested: nested}, nil
	}
	bashDef, err := render(parse(gateToolBashJSON))
	if err != nil {
		panic("forward: cannot render bash tool: " + err.Error())
	}
	readDef, err := render(parse(gateToolReadJSON))
	if err != nil {
		panic("forward: cannot render read tool: " + err.Error())
	}
	gateToolDefs = map[string]*gateToolDef{
		"bash": bashDef,
		"read": readDef,
	}
}
