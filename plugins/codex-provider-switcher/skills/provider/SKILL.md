---
name: provider
description: Inspect or switch the current Codex task's model provider through Codex Provider Switcher. Use only when the user explicitly invokes /provider status or /provider switch with one provider identifier such as openai or sub2api.
---

# Inspect Or Switch Provider

Accept exactly `/provider status` or `/provider switch <name>` as the
user-facing invocation. Desktop may encode the selected skill as a Markdown
link whose label is `$provider`. Submit only the requested status or switch
control, without adding prose, attachments, or other input items. The transport
switcher recognizes the exact Desktop encoding and handles it locally; do not
answer the control through a model or tool.
