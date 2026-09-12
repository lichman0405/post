# ADR-016：Ubuntu 24.04 LTS 为唯一 Canonical Development Environment

**Status:** Accepted

Supervisor、Worker、rddev、测试、CI 以 Ubuntu 24.04 LTS amd64 为标准。Windows Native 不在支持矩阵；Windows 用户使用 WSL2 且仓库放 Linux filesystem。目标是消除 shell/path/permission/process/Docker/Git 行为分叉。
