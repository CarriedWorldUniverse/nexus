#!/usr/bin/env node
/**
 * PreToolUse hook — defense in depth for nexus aspects.
 *
 * Aspects run claude-code with --permission-mode bypassPermissions
 * (bridle/provider/claudecode/claudecode.go), so claude-code's
 * built-in classifier is OFF. This hook is the sole gate on:
 *   1. Destructive bash patterns
 *   2. Writes to protected paths
 *   3. Writes containing credentials/secrets
 *
 * Fails closed on error: if the hook crashes, the action is denied
 * rather than silently allowed.
 *
 * Provisioned per-aspect by nexus/autospawn/provision.go, which
 * writes <aspect-home>/.claude/settings.json pointing at this script.
 *
 * Ported from agent-network/code/hooks/approve-tool.js, where the
 * same patterns have been gating live agents for months.
 */

const BLOCKED_COMMANDS = [
  /rm\s+(-[a-z]*f[a-z]*\s+)?\/(?!\w)/i,
  /rm\s+-[a-z]*r[a-z]*\s+[~.]\/?$/,
  // --force-with-lease is allowed (refuses if remote moved); raw --force blocked.
  /git\s+push\s+(?!.*--force-with-lease).*--force(?!-with-lease)\b/,
  /git\s+push\s+.*(?<![a-zA-Z0-9])-f\b/,
  /git\s+reset\s+--hard\s+origin\/main/,
  /git\s+clean\s+-[a-z]*f[a-z]*d/,
  /DROP\s+(TABLE|DATABASE|SCHEMA)/i,
  /TRUNCATE\s+TABLE/i,
  /DELETE\s+FROM\s+\S+\s*$/i,
  /dd\s+if=/,
  /mkfs\./,
  /chmod\s+(-R\s+)?777/,
  /:(){ :\|:& };:/,
  />\s*\/dev\/sda/,
  /format\s+[a-z]:/i,
  /reg\s+delete\s+HKLM/i,
  /shutdown\s+\/(s|r|f)/i,
  /Stop-Computer|Restart-Computer/i,
  /npm\s+publish/,
];

const BLOCKED_PATHS = [
  /^C:\\Windows/i,
  /^C:\\Program Files/i,
  /^\/etc\//,
  /^\/usr\//,
  /\.env$/,
  /credentials\.json$/,
  /\.pem$/,
  /id_rsa/,
];

const SECRET_PATTERNS = [
  { pattern: /AKIA[0-9A-Z]{16}/, label: "AWS access key" },
  { pattern: /sk-[a-zA-Z0-9]{20,}/, label: "OpenAI/Anthropic API key" },
  { pattern: /sk_live_[a-zA-Z0-9]+/, label: "Stripe live key" },
  { pattern: /sk_test_[a-zA-Z0-9]+/, label: "Stripe test key" },
  { pattern: /ghp_[a-zA-Z0-9]{36}/, label: "GitHub PAT" },
  { pattern: /gho_[a-zA-Z0-9]{36}/, label: "GitHub OAuth token" },
  { pattern: /github_pat_[a-zA-Z0-9_]+/, label: "GitHub fine-grained PAT" },
  { pattern: /glpat-[a-zA-Z0-9\-_]{20,}/, label: "GitLab PAT" },
  { pattern: /-----BEGIN (RSA|EC|DSA|OPENSSH) PRIVATE KEY-----/, label: "Private key" },
  { pattern: /xox[bpors]-[a-zA-Z0-9\-]+/, label: "Slack token" },
  { pattern: /hooks\.slack\.com\/services\/T[A-Z0-9]+\/B[A-Z0-9]+\/[a-zA-Z0-9]+/, label: "Slack webhook" },
  { pattern: /SG\.[a-zA-Z0-9_\-]{22}\.[a-zA-Z0-9_\-]{43}/, label: "SendGrid key" },
  { pattern: /[a-f0-9]{32}-us[0-9]{1,2}/, label: "Mailchimp API key" },
  { pattern: /sq0[a-z]{3}-[a-zA-Z0-9\-_]{22,}/, label: "Square token" },
  { pattern: /eyJ[a-zA-Z0-9_\-]{50,}\.eyJ[a-zA-Z0-9_\-]{50,}/, label: "JWT token (long)" },
  { pattern: /AZURE_[A-Z_]+=\s*['"][^'"]{20,}['"]/, label: "Azure credential" },
  { pattern: /password\s*[:=]\s*['"][^'"]{8,}['"]/i, label: "Hardcoded password" },
  { pattern: /secret\s*[:=]\s*['"][^'"]{8,}['"]/i, label: "Hardcoded secret" },
];

function deny(reason) {
  process.stdout.write(JSON.stringify({
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      reason
    }
  }));
}

function allow() {
  process.stdout.write(JSON.stringify({
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "allow"
    }
  }));
}

let input = "";
process.stdin.on("data", (chunk) => { input += chunk; });
process.stdin.on("end", () => {
  try {
    const context = JSON.parse(input);
    const tool = context.tool_name || "";
    const toolInput = context.tool_input || {};

    if (tool === "Bash") {
      const cmd = toolInput.command || "";
      for (const pattern of BLOCKED_COMMANDS) {
        if (pattern.test(cmd)) {
          return deny("Blocked: destructive command pattern");
        }
      }
    }

    if (tool === "Write" || tool === "Edit") {
      const filePath = toolInput.file_path || "";
      for (const pattern of BLOCKED_PATHS) {
        if (pattern.test(filePath)) {
          return deny("Blocked: protected file path");
        }
      }
      const content = toolInput.content || toolInput.new_string || "";
      for (const { pattern, label } of SECRET_PATTERNS) {
        if (pattern.test(content)) {
          return deny(`Blocked: content contains ${label}`);
        }
      }
    }

    allow();
  } catch (e) {
    console.error(`[approve-tool] hook error, failing closed: ${e && e.stack ? e.stack : e}`);
    deny(`hook error — denied by default. See stderr. (${e && e.message ? e.message : "unknown"})`);
  }
});
