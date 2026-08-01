---
name: provider
description: Switch the current Codex task to a configured model provider through Codex Provider Switcher. Use only when the user explicitly invokes $provider with one provider identifier such as openai or sub2api.
---

# Switch Provider

Accept exactly one provider identifier after `$provider`. Submit the control
input as `$provider <name>` without adding prose, attachments, or other input
items. The transport switcher performs the change locally and returns the
result; do not call a model or tool for the switch.
