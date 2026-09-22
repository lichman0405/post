#!/usr/bin/env bash
#
# V1 最终验收审计脚本（T1207 final audit）
#
# 这个脚本做四件事：
#   1. 统计 v1_required=true 且未 merged 的任务，排除 T1207 自身；
#   2. 统计 blocking=true 与各 status 的测试条数；
#   3. 校验 tests/acceptance/v1-final-report.md 是否包含所有强制标记，
#      且报告里的数字/名单/基准 commit/计数块与台账、与 git 逐条一致；
#   4. 显式断言台账里 failed / skipped 为 0（分布标记之外的第二道，直接数）。
#
# 它是**基准快照**（snapshot）校验器，不是常驻检查：报告是对某一棵树的证书。
# 报告的「生成基准」与当前 HEAD 不符时（合并、落账或任何一次 HEAD 前进之后必然如此），
# 它打印一段专门的说明——报告钉在哪个 SHA、当前树是哪个 SHA、两条出路——
# 并**照样以非 0 退出**。快照校验器不是提示器，断言不因为"这是正常的"而放宽。
#
# --emit-tables: 生成**够重建报告的那一份**产物：生成注释行、生成基准行、
#   两张表、计数块。报告里这些只许这样生成，不许手写——手写的名单骗得过条数检查，
#   骗不过第 4 节的名单检查；手写的计数块骗不过第 3 节的计数块校验。

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
REPORT="$ROOT/tests/acceptance/v1-final-report.md"

if ! command -v jq >/dev/null 2>&1; then
  echo "FAIL: jq is required" >&2
  exit 1
fi

EMIT_TABLES=0
for arg in "$@"; do
  case "$arg" in
    --emit-tables) EMIT_TABLES=1 ;;
    *) echo "FAIL: unknown argument: $arg" >&2; exit 1 ;;
  esac
done

fail=0

# ---------------------------------------------------------------------------
# 1. v1_required 未 merged 任务（排除 T1207）
# ---------------------------------------------------------------------------
EXCLUDE_TASK="T1207"

unmerged_json="$(jq -s '
  .[0].tasks as $defs |
  .[1].tasks as $states |
  [ $defs[] | select(.v1_required==true) |
    .id as $id |
    {id: $id, status: ($states[$id].status // "missing"), phase, title}
  ] |
  map(select(.status != "merged" and .id != "'"$EXCLUDE_TASK"'")) |
  sort_by(.id)
' "$ROOT/tasks/tasks.json" "$ROOT/tasks/task_status.json")"

unmerged_count="$(echo "$unmerged_json" | jq 'length')"
v1_total="$(jq '[.tasks[] | select(.v1_required==true)] | length' "$ROOT/tasks/tasks.json")"

# ---------------------------------------------------------------------------
# 2. blocking / not_run / 各 status 的测试条数
# ---------------------------------------------------------------------------
not_run_json="$(jq '.tests | map(select(.blocking == true and .status == "not_run")) | sort_by(.id)' "$ROOT/tasks/tests.json")"
not_run_count="$(echo "$not_run_json" | jq 'length')"

total_tests="$(jq '[.tests[]] | length' "$ROOT/tasks/tests.json")"
passed_tests="$(jq '[.tests[] | select(.status=="passed")] | length' "$ROOT/tasks/tests.json")"
failed_tests="$(jq '[.tests[] | select(.status=="failed")] | length' "$ROOT/tasks/tests.json")"
skipped_tests="$(jq '[.tests[] | select(.status=="skipped")] | length' "$ROOT/tasks/tests.json")"
blocking_tests="$(jq '[.tests[] | select(.blocking==true)] | length' "$ROOT/tasks/tests.json")"

# ---------------------------------------------------------------------------
# --emit-tables：生成够重建报告的那一份产物（生成物，不是断言）
# ---------------------------------------------------------------------------
if [ "$EMIT_TABLES" = "1" ]; then
  emit_head="$(git -C "$ROOT" rev-parse HEAD)"
  echo "<!-- 生成命令：bash tests/acceptance/v1-final-audit.sh --emit-tables -->"
  echo
  echo "> 生成基准：\`$emit_head\`"
  echo
  echo "| 任务 | 状态 | 阶段 | 标题 |"
  echo "|------|------|------|------|"
  if [ "$unmerged_count" = "0" ]; then
    echo "| （无） | — | — | v1_required=true 且非 merged 的任务 0 笔（已排除 T1207 自身） |"
  else
    echo "$unmerged_json" | jq -r '.[] | "| \(.id) | \(.status) | \(.phase) | \(.title) |"'
  fi
  echo
  echo "| 测试 ID | 任务 | 名称 |"
  echo "|---------|------|------|"
  if [ "$not_run_count" = "0" ]; then
    echo "| （无） | — | — |"
  else
    echo "$not_run_json" | jq -r '.[] | "| \(.id) | \(.task_id) | \(.name) |"'
  fi
  echo
  echo "<!-- 计数块：报告引用的每个数字都由脚本给出，不许手写 -->"
  echo '```text'
  echo "COUNTS v1_required_total=$v1_total"
  echo "COUNTS v1_required_unmerged=$unmerged_count"
  echo "COUNTS excluded=$EXCLUDE_TASK"
  echo "COUNTS tests_total=$total_tests"
  echo "COUNTS tests_passed=$passed_tests"
  echo "COUNTS tests_not_run=$not_run_count"
  echo "COUNTS tests_failed=$failed_tests"
  echo "COUNTS tests_skipped=$skipped_tests"
  echo "COUNTS tests_blocking=$blocking_tests"
  echo '```'
  exit 0
fi

echo "EXCLUDED=$EXCLUDE_TASK"
echo "V1_REQUIRED_UNMERGED_COUNT=$unmerged_count"
echo "V1_REQUIRED_UNMERGED_LIST:"
echo "$unmerged_json" | jq -r '.[] | "  - \(.id) [\(.status)] (\(.phase)) \(.title)"'

echo "BLOCKING_NOT_RUN_COUNT=$not_run_count"
echo "$not_run_json" | jq -r '.[] | "  - \(.id) (\(.task_id)) \(.name)"'

# ---------------------------------------------------------------------------
# 3. 校验报告文件与强制标记
# ---------------------------------------------------------------------------
if [ ! -f "$REPORT" ]; then
  echo "FAIL: report not found: $REPORT" >&2
  exit 1
fi

require_marker() {
  local marker="$1"
  if ! grep -qF "$marker" "$REPORT"; then
    echo "FAIL: report missing required marker: $marker" >&2
    fail=1
  fi
}

# 10 个 Gate 标题
for gate in "Gate A" "Gate B" "Gate C" "Gate D" "Gate E" "Gate F" "Gate G" "Gate H" "Gate I" "Development System Gate"; do
  require_marker "### $gate"
done

# 剩余风险编号
for r in R1 R2 R3 R4 R5 R6 R7 R8 R9 R10 R11; do
  require_marker "### $r."
done

# 关键事实标记（对应四条已知缺口 + T1206 缺席清单）
require_marker "产品的 blob 写入路径缺失"
require_marker "reopen 在生产里没有入口"
require_marker "部署的 search 没有接任何 planner / embedder / answer provider"
require_marker '`rddev worker collect` 的证据没说清它评了什么'
require_marker "SAST、容器扫描、SBOM 三项缺席"

# search 接线缺口的可复跑证据（R5）：报告必须给出具体命令与命中行，
# 不接受「grep 过、没 wire」这类无法复现的措辞
require_marker "NewRetriever(retrievalStore, nil)"
require_marker "answer.New(answer.Deps"

# 「全部 passed」类全称断言的核对命令必须留在报告的复跑清单里
require_marker "# 7. 报告里「全部 passed」类全称断言的核对"

# 关键结论标记（随事实更新的那一句；旧的「不能宣告完成」在台账条件满足后已不成立，
# 换成本轮的实际结论句，标记本身不放松：报告若不写这句，脚本就红）
require_marker "V1 的两条台账条件在本基准 commit 上已经满足"
require_marker "EXCLUDED=T1207"
require_marker "${not_run_count} 条 blocking 测试为"

# 报告的基准 commit 必须就是当前 HEAD（数字与 SHA 都不许抄旧树）
report_head="$(grep -oE '^> 生成基准：`[0-9a-f]{40}`' "$REPORT" | head -n1 | grep -oE '[0-9a-f]{40}')"
actual_head="$(git -C "$ROOT" rev-parse HEAD)"
if [ -z "$report_head" ]; then
  echo "FAIL: report does not state its baseline commit in the expected format" >&2
  fail=1
elif [ "$report_head" != "$actual_head" ]; then
  # 基准不符不是「报告写错了」，是「证书过期了」——但这仍然是一次失败。
  cat >&2 <<EOF
FAIL: 报告的基准不是这棵树。

  本审计是【基准快照】（snapshot），不是常驻检查：报告是对某一棵树的证书，
  它钉住的每个数字（v1_required 清单、tests.json 分布、blocking not_run 名单）
  都是在下面那个 SHA 上取的一次快照。HEAD 前进一次（合并、落账、任何一次提交），
  报告就与当前树不同步——这不是报告错了，是它记录的那棵树不是这一棵。

  报告钉在的基准：$report_head
  当前工作树 HEAD：$actual_head

  两条出路：
    (a) 在报告自己的基准上跑这条命令：git checkout $report_head（或那个基准的 worktree），
        再执行 bash tests/acceptance/v1-final-audit.sh；
    (b) 已经在基准树上核对完判定与证据之后，把报告移到当前树：
        bash tests/acceptance/v1-final-audit.sh --emit-tables
        把输出的「生成基准行」贴回报告头部、「两张表」贴回第 1.2 / 1.3 节、
        「计数块」贴回第 1.3 节（判定与证据由人按台账改，数字只许来自脚本），
        再造这一棵树的证书。

  退出码照样非 0：这一条是快照校验，不是提示。
EOF
  fail=1
fi

# 基准行必须是报告开头的那一条（不是正文引用里的某一条）：
# 第 3 行内的头部行才是「这份报告钉在哪」的声明。
first_section_line="$(grep -nE '^## 1\.' "$REPORT" | head -n1 | cut -d: -f1)"
head_line="$(grep -nE '^> 生成基准：`[0-9a-f]{40}`' "$REPORT" | head -n1 | cut -d: -f1)"
if [ -z "$head_line" ] || [ -z "$first_section_line" ] || [ "$head_line" -ge "$first_section_line" ]; then
  echo "FAIL: the report's baseline line must be in the header block, before the first section (line $head_line vs $first_section_line)" >&2
  fail=1
fi

# 校验报告中的计数与脚本计算一致（防止报告悄悄改数字）
report_unmerged_count="$(grep -E '^\*\*计数：[0-9]+ / 0' "$REPORT" | grep -oE '[0-9]+' | head -n1)"
if [ -z "$report_unmerged_count" ]; then
  echo "FAIL: report does not state unmerged count in expected format" >&2
  fail=1
elif [ "$report_unmerged_count" != "$unmerged_count" ]; then
  echo "FAIL: report claims $report_unmerged_count unmerged tasks, script counted $unmerged_count" >&2
  fail=1
fi

report_not_run_count="$(grep -E '^当前结果：\*\*[0-9]+ 条 blocking 测试为' "$REPORT" | grep -oE '[0-9]+' | head -n1)"
if [ -z "$report_not_run_count" ]; then
  echo "FAIL: report does not state not_run count in expected format" >&2
  fail=1
elif [ "$report_not_run_count" != "$not_run_count" ]; then
  echo "FAIL: report claims $report_not_run_count not_run tests, script counted $not_run_count" >&2
  fail=1
fi

# 校验报告声明的 tests.json 整体分布与台账一致
distribution_marker="**${total_tests} 条 = ${passed_tests} \`passed\` + ${not_run_count} \`not_run\`**"
require_marker "$distribution_marker"

# 上面那一条只要求「正确的分布句式**至少出现一次**」——一份在第 1.3 节写对、
# 却在第 0 节写错的报告能骗过它（变异 M8 实测逃逸）。所以这里把**所有**该句式的
# 出现都收上来：集合里只许有一个元素，且就是台账给的那一个。
# 「说对一次」不算数，得「每一处都说对」。
dist_found="$(grep -oE '\*\*[0-9]+ 条 = [0-9]+ `passed` \+ [0-9]+ `not_run`\*\*' "$REPORT" | sort -u)"
if [ "$dist_found" != "$distribution_marker" ]; then
  echo "FAIL: the report's ledger-distribution statements are not exactly the one the ledger gives" >&2
  echo "--- report says ---" >&2
  echo "$dist_found" >&2
  echo "--- ledger says ---" >&2
  echo "$distribution_marker" >&2
  fail=1
fi

if [ "$blocking_tests" != "$total_tests" ]; then
  echo "FAIL: ledger has non-blocking entries (blocking=$blocking_tests, total=$total_tests); report claims every entry is blocking" >&2
  fail=1
fi

# 分布的第二道断言：failed / skipped 必须为 0。
# 上面的分布标记是字符串相等，理论上能被「凑数」绕过（多写一条 failed、
# 同时把 passed 的数字改到自洽）；这一条直接数台账，不再经过报告。
if [ "$failed_tests" != "0" ] || [ "$skipped_tests" != "0" ]; then
  echo "FAIL: ledger holds $failed_tests failed / $skipped_tests skipped entries; the report claims none" >&2
  fail=1
fi

# 报告引用的每个数字都必须在脚本生成的计数块里（--emit-tables 的产物，
# 逐条比对；报告手写一个数字而与台账不符，这里就红）
while IFS= read -r counts_line; do
  require_marker "$counts_line"
done <<EOF
COUNTS v1_required_total=$v1_total
COUNTS v1_required_unmerged=$unmerged_count
COUNTS excluded=$EXCLUDE_TASK
COUNTS tests_total=$total_tests
COUNTS tests_passed=$passed_tests
COUNTS tests_not_run=$not_run_count
COUNTS tests_failed=$failed_tests
COUNTS tests_skipped=$skipped_tests
COUNTS tests_blocking=$blocking_tests
EOF

# ---------------------------------------------------------------------------
# 4. 名单一致性：只钉条数不够——把「谁在名单里」也钉上
#    （复核员的反例：把五笔任务换成不存在的 T9999… 而条款数字不动，脚本照样通过）
# ---------------------------------------------------------------------------

# 4a. 报告的未合并任务清单（### 1.2 到 ### 1.3 之间的表格）必须与脚本算出的
#     名单逐条相等，且每条的状态列也要对得上。
computed_tasks="$(echo "$unmerged_json" | jq -r '.[] | "\(.id) \(.status)"' | sort)"
report_tasks="$(awk '/^### 1\.2 /,/^### 1\.3 /' "$REPORT" | grep -oE '^\| T[0-9]{4} \| [a-z_]+ ' | sed -E 's/^\| (T[0-9]{4}) \| ([a-z_]+) $/\1 \2/' | sort)"

if [ "$report_tasks" != "$computed_tasks" ]; then
  echo "FAIL: report task list != ledger list" >&2
  echo "--- report says ---" >&2
  echo "$report_tasks" >&2
  echo "--- ledger says ---" >&2
  echo "$computed_tasks" >&2
  fail=1
fi

# 4b. 报告的 not_run 清单（当前结果 到 账本规则 之间的表格）必须与脚本算出的
#     id 集合相等（双向：不多、不少）。
computed_not_run="$(echo "$not_run_json" | jq -r '.[].id' | sort)"
report_not_run="$(awk '/^当前结果：/,/^> 账本规则/' "$REPORT" | grep -oE '^\| T[0-9]{4}-TEST-[0-9]{2} ' | grep -oE 'T[0-9]{4}-TEST-[0-9]{2}' | sort)"

if [ "$report_not_run" != "$computed_not_run" ]; then
  echo "FAIL: report not_run table != ledger not_run list" >&2
  echo "--- only in report ---" >&2
  comm -23 <(echo "$report_not_run") <(echo "$computed_not_run") >&2
  echo "--- only in ledger ---" >&2
  comm -13 <(echo "$report_not_run") <(echo "$computed_not_run") >&2
  fail=1
fi

# 4c. 两个名单的行数与脚本算出的条数一致（防止「表里多一行、计数不改」）
report_task_rows="$(echo "$report_tasks" | grep -c .)"
report_not_run_rows="$(echo "$report_not_run" | grep -c .)"
if [ "$report_task_rows" != "$unmerged_count" ]; then
  echo "FAIL: report task table has $report_task_rows rows, script counted $unmerged_count" >&2
  fail=1
fi
if [ "$report_not_run_rows" != "$not_run_count" ]; then
  echo "FAIL: report not_run table has $report_not_run_rows rows, script counted $not_run_count" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "AUDIT FAILED" >&2
  exit 1
fi

echo "AUDIT OK: report markers present, counts match ($unmerged_count unmerged, $not_run_count not_run, $failed_tests failed, $skipped_tests skipped)."
exit 0
