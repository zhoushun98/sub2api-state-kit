# 变更记录

## 0.1.2 · 2026-09-19

- 新增全局「新账号默认开启 STATE」（默认套餐、默认目标模型）；采集循环每 6 秒认领开启后创建、已绑固定代理、未手动配置过票据的 OpenAI OAuth 账号。
- 设置键 `openai_codex_ticket_default_enabled` / `_enabled_since` / `_plan` / `_models`；API 合约快照同步更新。

## 0.1.1 · 2026-09-19

- 账号级票据支持多模型：`gpt-6-astra` 与 `gpt-5.6-sol` 各自采集、复验、注入；缺票严格拦截该模型。
- 采集任务按「账号 × 模型」登记；状态接口新增 `models` / `model_statuses`；面板改为两个复选框。
- 仅增删模型时保留未变模型的票据与 revision。

## 0.1.0 · 2026-09-18

- 原作者 wangyunjeff 发布的账号级 STATE 扩展。
