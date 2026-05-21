# URL Text Fetcher MCP for LM Studio

Go MCP stdio server for LM Studio that searches the web and fetches readable text or page links from public internet URLs.

Tested on macOS Sequoia with Gemma-4-e4b, works nicely. Code thrown by GPT 5.5 codex.

Requires Go 1.26. MIT licensed.

## Install

```sh
sudo make install
```

Installs `/usr/local/bin/url-text-fetcher`.

## LM Studio Config

use this in mcp setup (hammer icon, then plus icon).

```json
{
  "mcpServers": {
    "url-text-fetcher": {
      "command": "/usr/local/bin/url-text-fetcher"
    }
  }
}
```

## Tools

- `fetch_url_text`: fetches text from a public `http://` or `https://` URL.
- `fetch_page_links`: returns navigable links as `{ "text": "...", "url": "..." }` objects.
- `fetch_search_results`: searches DuckDuckGo Lite and returns `{ "title": "...", "url": "..." }` objects.

Local/private network targets, unsafe redirects, non-text content, and oversized responses are blocked.
