# ADR-019：Gitea 仅作为内部 Git Infrastructure

**Status:** Accepted

不从零实现 Git smart HTTP/SSH/LFS，不 fork Gitea，不暴露其 UI。产品的 Project/User/Org/Research PR/Release 均是独立领域对象，通过 GitProvider mapping 到 Gitea。
