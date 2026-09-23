# 数据分类

V1 分类：

- PUBLIC：明确发布/公开的 RSG metadata、public assets/knowledge/profile。
- INTERNAL：登录用户可见但非公开网络内容（V1 少用）。
- PROJECT_PRIVATE：私有 Project/Branch/RSG。
- RESTRICTED_BLOB：metadata 可公开但数据本体受限。
- SECRET：token/password/webhook secret/keys，不进入科研对象。

规则：PUBLIC 是显式状态；默认新 private project 内容为 PROJECT_PRIVATE。任何分类向更公开方向移动都产生 governance event。SECRET 永远不进入普通 logs/search/vector embedding。

**出平台规则（`L3-⓪` 裁定，2026-09-23）**：V1 **不把任何分类的内容发送到平台之外的第三方
服务**（模型、向量、分析）——包括 PUBLIC。接口位置保留、测试用确定性假件、零联网；接线与
「哪一级数据永不出平台」的完整边界是 V1.x 的议题（`docs/02` §4、`tasks/decisions.md` ㊻）。
