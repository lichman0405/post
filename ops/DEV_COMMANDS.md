# Dev Commands（目标接口）

开发环境完成 P0 后应支持：

```bash
make bootstrap
rddev doctor
rddev env up
make dev
make test
make test-integration
make test-e2e
rddev task next
rddev worker spawn Txxxx
rddev worker collect Txxxx
rddev task verify Txxxx
rddev task accept Txxxx
rddev git commit Txxxx
rddev pr open Txxxx
rddev pr merge Txxxx
```

Supervisor 使用 `rddev` 管 Worker/Git；Worker 不直接执行 `rddev git/pr` control-plane 命令。

## 环境预检（当前可用，`rddev doctor` 落地前）

从仓库根目录运行：

```bash
./ops/doctor.sh                    # 人类可读输出；退出码 0/1/2/3（见下）
./ops/doctor.sh --json             # 机器可读 JSON（ops/doctor-output.schema.json）
./ops/doctor.sh --check-docker-daemon   # 额外探测 docker daemon 与 data-root 磁盘（默认关闭，需 socket 权限）
bash ops/tests/doctor-unit-test.sh  # fixture 驱动的决策逻辑单元测试（bash + python3）
bash ops/tests/doctor-smoke-test.sh # 宿主端到端自洽性冒烟测试（bash + python3）
```

退出码契约：`0` = required 检查全过（advisory 警告不影响）；`1` = 工具链缺失或版本超出基线（显式漂移报告，不自动修复）；`2` = 非 canonical 环境（非 Linux / Windows Native / 非 Ubuntu 24.04 / 非 amd64）；`3` = 用法错误。

检查契约（id、severity、baseline、remediation）见 `ops/doctor-checks.md` —— 这是 T0009 实现 `rddev doctor` 的规范，不可在实现时重新发明检查语义。
