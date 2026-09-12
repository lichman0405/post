# ADR-003：V1 使用内部 Gitea 提供 Git transport
Status: Accepted

## Decision
不从零实现 smart HTTP/SSH/ref storage。Gitea 仅作为内部 Git backend；自研平台承担产品 UI、RSG、权限、PR、Review、Release、Network。

## Exit
通过 GitPort adapter 隔离，未来可换其他 backend。
