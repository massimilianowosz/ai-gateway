#!/usr/bin/env python3
"""Generate the console's brand icon sprite.

Two sources, in order of preference:

  logos         SVG Logos via @iconify-json/logos, ~2170 full-colour marks.
                This is the primary set because Simple Icons keeps removing
                brands over trademark policy - Slack, Salesforce, Microsoft,
                Adobe, Twilio, SendGrid, Playwright and OpenAI are all absent
                there and present here.
  simple-icons  the monochrome set bundled with react-icons, for the handful
                of brands SVG Logos lacks.

Neither is fetched at runtime: the console is CSP-locked and offline, so this
runs once and the generated sprite is committed.

    npm --prefix /tmp/iconsrc install @iconify-json/logos
    python3 scripts/gen_brand_icons.py

Colour marks keep their own fills and carry class="brand-colour"; monochrome
marks inherit currentColor so the tone classes still tint them.

The logos remain their owners' trademarks and are used only to identify the
service a session talked to, which is nominative use.
"""
import json
import os
import re
import sys

LOGOS = "/tmp/iconsrc/node_modules/@iconify-json/logos/icons.json"
SIMPLE = os.path.expanduser(
    "~/code/modelhive/portal/node_modules/react-icons/si/index.mjs"
)
OUT = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    "internal", "console", "assets", "brands.svg",
)

# sprite id -> (name in SVG Logos, component in react-icons' Simple Icons).
# Either may be None; the first that resolves wins.
ICONS = {
    # --- agents, runtimes and model vendors ---
    "b-claude": ("claude-icon", "SiClaude"),
    "b-claude-code": (None, "SiClaudecode"),
    "b-anthropic": ("anthropic-icon", "SiAnthropic"),
    "b-openai": ("openai-icon", None),
    "b-copilot": ("github-copilot", "SiGithubcopilot"),
    "b-github": ("github-icon", "SiGithub"),
    "b-python": ("python", "SiPython"),
    "b-node": ("nodejs-icon", "SiNodedotjs"),
    "b-go": ("go", "SiGo"),
    "b-rust": ("rust", "SiRust"),
    "b-curl": (None, "SiCurl"),
    "b-chrome": ("chrome", "SiGooglechrome"),
    "b-cursor": ("cursor", "SiCursor"),
    "b-zed": (None, "SiZedindustries"),
    "b-gemini": ("google-gemini", "SiGooglegemini"),
    "b-langchain": ("langchain-icon", "SiLangchain"),
    "b-postman": ("postman-icon", "SiPostman"),
    "b-insomnia": ("insomnia-icon", "SiInsomnia"),
    "b-windsurf": (None, "SiWindsurf"),
    "b-mcp": (None, "SiModelcontextprotocol"),
    "b-deepseek": ("deepseek-icon", "SiDeepseek"),
    "b-qwen": (None, "SiQwen"),
    "b-mistral": ("mistral-ai-icon", "SiMistralai"),
    "b-meta": ("meta-icon", "SiMetaai"),
    "b-perplexity": (None, "SiPerplexity"),
    "b-ollama": ("ollama", "SiOllama"),
    "b-huggingface": ("hugging-face-icon", "SiHuggingface"),
    "b-moonshot": (None, "SiMoonshotai"),
    "b-vercel": ("vercel-icon", "SiVercel"),
    "b-google": ("google-icon", "SiGoogle"),

    # --- connectors: the Claude directory's featured set ---
    "b-googledrive": ("google-drive", "SiGoogledrive"),
    "b-gmail": ("google-gmail", "SiGmail"),
    "b-googlecalendar": ("google-calendar", "SiGooglecalendar"),
    "b-googlesheets": ("google-sheets", "SiGooglesheets"),
    "b-googledocs": (None, "SiGoogledocs"),
    "b-googleanalytics": ("google-analytics", "SiGoogleanalytics"),
    "b-canva": ("canva-icon", None),
    "b-microsoft": ("microsoft-icon", None),
    "b-sharepoint": ("microsoft-sharepoint", None),
    "b-onedrive": ("microsoft-onedrive", None),
    "b-outlook": ("microsoft-outlook", None),
    "b-teams": ("microsoft-teams", None),
    "b-notion": ("notion-icon", "SiNotion"),
    "b-figma": ("figma", "SiFigma"),
    "b-slack": ("slack-icon", None),
    "b-atlassian": ("atlassian", "SiAtlassian"),
    "b-hubspot": ("hubspot", "SiHubspot"),
    "b-asana": ("asana-icon", "SiAsana"),
    "b-linear": ("linear-icon", "SiLinear"),
    "b-supabase": ("supabase-icon", "SiSupabase"),
    "b-indeed": (None, "SiIndeed"),
    "b-adobe": ("adobe", None),
    "b-monday": ("monday-icon", None),
    "b-spotify": ("spotify-icon", "SiSpotify"),
    "b-shopify": ("shopify", "SiShopify"),
    "b-intercom": ("intercom-icon", "SiIntercom"),
    "b-miro": ("miro-icon", "SiMiro"),
    "b-zapier": ("zapier-icon", "SiZapier"),

    # --- connectors: the modelhive MCP registry ---
    "b-postgresql": ("postgresql", "SiPostgresql"),
    "b-mysql": ("mysql-icon", "SiMysql"),
    "b-sqlite": ("sqlite", "SiSqlite"),
    "b-mongodb": ("mongodb-icon", "SiMongodb"),
    "b-redis": ("redis", "SiRedis"),
    "b-elasticsearch": ("elasticsearch", "SiElasticsearch"),
    "b-snowflake": ("snowflake-icon", "SiSnowflake"),
    "b-bigquery": ("google-bigquery", "SiGooglebigquery"),
    "b-gitlab": ("gitlab", "SiGitlab"),
    "b-bitbucket": ("bitbucket", "SiBitbucket"),
    "b-docker": ("docker-icon", "SiDocker"),
    "b-jenkins": ("jenkins", "SiJenkins"),
    "b-terraform": ("terraform-icon", "SiTerraform"),
    "b-aws": ("aws", None),
    "b-azure": ("microsoft-azure", None),
    "b-googlecloud": ("google-cloud", "SiGooglecloud"),
    "b-cloudflare": ("cloudflare-icon", "SiCloudflare"),
    "b-discord": ("discord-icon", "SiDiscord"),
    "b-zoom": ("zoom-icon", "SiZoom"),
    "b-confluence": ("confluence", "SiConfluence"),
    "b-dropbox": ("dropbox", "SiDropbox"),
    "b-box": ("box", "SiBox"),
    "b-sentry": ("sentry-icon", "SiSentry"),
    "b-datadog": ("datadog", "SiDatadog"),
    "b-grafana": ("grafana", "SiGrafana"),
    "b-pagerduty": ("pagerduty", "SiPagerduty"),
    "b-newrelic": ("new-relic-icon", "SiNewrelic"),
    "b-graphql": ("graphql", "SiGraphql"),
    "b-airtable": ("airtable", "SiAirtable"),
    "b-jira": ("jira", "SiJira"),
    "b-trello": ("trello", "SiTrello"),
    "b-clickup": ("clickup-icon", "SiClickup"),
    "b-salesforce": ("salesforce", None),
    "b-pipedrive": ("pipedrive", None),
    "b-zendesk": ("zendesk-icon", "SiZendesk"),
    "b-freshdesk": ("freshdesk", None),
    "b-stripe": ("stripe", "SiStripe"),
    "b-quickbooks": ("quickbooks", "SiQuickbooks"),
    "b-xero": ("xero", "SiXero"),
    "b-plaid": ("plaid", None),
    "b-brave": ("brave", "SiBrave"),
    "b-algolia": ("algolia", "SiAlgolia"),
    "b-mailchimp": ("mailchimp-freddie", "SiMailchimp"),
    "b-segment": ("segment-icon", None),
    "b-twilio": ("twilio-icon", None),
    "b-sendgrid": ("sendgrid-icon", None),
    "b-playwright": ("playwright", None),
    "b-puppeteer": ("puppeteer", "SiPuppeteer"),
    "b-1password": ("1password", "Si1Password"),
    "b-vault": ("hashicorp-vault-icon", "SiVault"),
    "b-obsidian": ("obsidian-icon", "SiObsidian"),
    "b-todoist": ("todoist-icon", "SiTodoist"),
    "b-netlify": ("netlify-icon", "SiNetlify"),
    "b-lucid": (None, "SiLucid"),
}


def logo_symbol(sprite_id, icons, defaults, name):
    """Build a symbol from SVG Logos, or None if the mark is unsuitable.

    Returns None for wordmarks. A few entries are the full lock-up rather than
    the bare mark and are unreadable in a 15px tile; the ratios separate them
    cleanly - pipedrive 4.4, adobe 3.8, sqlite 2.3 against aws 1.7, monday 1.6,
    salesforce 1.4 and everything square. Those fall through to the monochrome
    square, or to the category glyph when Simple Icons lacks them too.
    """
    icon = icons.get(name)
    if not icon:
        return None
    # left/top are present but null in this set, so .get with a default is not
    # enough - a None would land in the viewBox and invalidate it.
    w = icon.get("width") or defaults["width"]
    h = icon.get("height") or defaults["height"]
    left = icon.get("left") or 0
    top = icon.get("top") or 0
    if max(w, h) / min(w, h) > 1.8:
        return None
    return (f'<symbol id="{sprite_id}" class="brand-colour" '
            f'viewBox="{left} {top} {w} {h}">{unstyle(icon["body"])}</symbol>')


def unstyle(body):
    """Rewrite style attributes as presentation attributes.

    The console serves its own stylesheets under `style-src 'self'`, and that
    directive rejects style attributes as well as style elements - so a single
    `style="mask-type:alpha"` carried in from the upstream icon set is enough
    for the browser to drop the declaration and log a violation. Every property
    these marks use is also an SVG presentation attribute, so the same styling
    survives the move with no visual change.
    """

    def rewrite(match):
        attrs = []
        for decl in match.group(1).split(";"):
            if ":" not in decl:
                continue
            prop, _, value = decl.partition(":")
            attrs.append(f'{prop.strip()}="{value.strip()}"')
        return (" " + " ".join(attrs)) if attrs else ""

    return re.sub(r'\s*style="([^"]*)"', rewrite, body)


def simple_symbol(sprite_id, source, component):
    start = source.find(f"export function {component} (")
    if start < 0:
        return None
    end = source.find("export function ", start + 1)
    body = source[start:end if end > 0 else len(source)]
    blob = re.search(r"GenIcon\((\{.*\})\)\(props\)", body, re.S)
    if not blob:
        return None
    try:
        icon = json.loads(blob.group(1))
    except json.JSONDecodeError:
        return None

    paths = []

    def walk(node):
        if node.get("tag") == "path" and node.get("attr", {}).get("d"):
            paths.append(node["attr"]["d"])
        for child in node.get("child", []) or []:
            walk(child)

    walk(icon)
    if not paths:
        return None
    inner = "".join(f'<path d="{d}"/>' for d in paths)
    return (f'<symbol id="{sprite_id}" viewBox="0 0 24 24" '
            f'fill="currentColor">{inner}</symbol>')


def main():
    if not os.path.exists(LOGOS):
        sys.exit(f"SVG Logos not found at {LOGOS}\n"
                 "run: npm --prefix /tmp/iconsrc install @iconify-json/logos")
    data = json.load(open(LOGOS))
    icons = data["icons"]
    defaults = {"width": data.get("width") or 24, "height": data.get("height") or 24}

    simple_src = open(SIMPLE).read() if os.path.exists(SIMPLE) else ""

    symbols, colour, mono, missing = [], 0, 0, []
    for sprite_id, (logo_name, component) in sorted(ICONS.items()):
        sym = logo_symbol(sprite_id, icons, defaults, logo_name) if logo_name else None
        if sym:
            colour += 1
        elif component and simple_src:
            sym = simple_symbol(sprite_id, simple_src, component)
            if sym:
                mono += 1
        if sym:
            symbols.append(sym)
        else:
            missing.append(sprite_id)

    header = ("<!-- Generated by scripts/gen_brand_icons.py - do not edit by hand.\n"
              "     Colour marks from SVG Logos, monochrome fallbacks from Simple\n"
              "     Icons. Logos remain the trademarks of their owners and identify\n"
              "     the service a session talked to. -->\n")
    svg = (header
           + '<svg xmlns="http://www.w3.org/2000/svg" class="icon-sprite" aria-hidden="true">\n  '
           + "\n  ".join(symbols) + "\n</svg>\n")
    with open(OUT, "w") as fh:
        fh.write(svg)

    print(f"wrote {len(symbols)} symbols to {os.path.relpath(OUT)}")
    print(f"  {colour} full colour, {mono} monochrome fallback")
    if missing:
        print("  no source for:", ", ".join(missing))


if __name__ == "__main__":
    main()
