---
name: provider
description: Inspect or switch the current Codex task's model provider through Codex Provider Switcher. Use only when the user explicitly invokes $provider status or $provider switch with one provider identifier such as openai or sub2api.
---

# Inspect Or Switch Provider

Accept exactly `$provider status` or `$provider switch <name>`. Submit that
control input without adding prose, attachments, or other input items. The
transport switcher handles the query or change locally and returns the result;
do not call a model or tool for either operation.
