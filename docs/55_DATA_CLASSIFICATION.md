# 数据分类

V1 分类：

- PUBLIC：明确发布/公开的 RSG metadata、public assets/knowledge/profile。
- INTERNAL：登录用户可见但非公开网络内容（V1 少用）。
- PROJECT_PRIVATE：私有 Project/Branch/RSG。
- RESTRICTED_BLOB：metadata 可公开但数据本体受限。
- SECRET：token/password/webhook secret/keys，不进入科研对象。

规则：PUBLIC 是显式状态；默认新 private project 内容为 PROJECT_PRIVATE。任何分类向更公开方向移动都产生 governance event。SECRET 永远不进入普通 logs/search/vector embedding。
