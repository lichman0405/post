#!/usr/bin/env bash
#
# V1 最终验收审计脚本（T1207 起，T1210 刷新，T1213 重钉）
#
# 这个脚本做六件事（T1213 复核 nit：这里原写「五件事」，而下面的清单在 T1210
# 加进第 5、6 条之后已经是六条——数一遍清单就知道，改的是这句注释）：
#   1. 统计 v1_required=true 且未 merged 的任务，排除 T1207 自身；
#   2. 统计 blocking=true 与各 status 的测试条数，并把**计数块原样打印**——
#      普通运行与 --emit-tables 走同一个函数：一个供人读，一个供重建报告，
#      读到的就是被校验的那一份；
#   3. 校验 tests/acceptance/v1-final-report.md 是否包含所有强制标记，
#      且报告里的数字/名单/基准 commit/计数块与台账、与 git 逐条一致；
#   4. 显式断言台账里 failed / skipped 为 0，以及**每一条 passed 都带 evidence**
#      （分布标记之外的第二、第三道，直接数台账，不经过报告）；
#   5. 报告里每一处「同一个事实」的出现都必须说对——句式的字面相等不够，
#      把**全部**出现收成集合、要求集合里只有一个元素（T1210 要求 4d）：
#      说对一次不算数，得每一处都说对。适用对象包括登记数两句、台账分布那一句、
#      以及**基准行**（头部说对、正文留一条旧 SHA 也算错）；
#   6. 两个「不许手写」的计数：§0 的 L3 由报告与台账两边的编号集合数出来
#      （两边必须相等），**每一条还要有自己的 `**裁定**：` 行**（行数 == 点名数，
#      T1214）；门层的「几通过 / 几未通过」由报告自己的处置行数出来——判定与统计
#      必须一致（T1210 要求 4b），**未通过的门名只在未通过数 > 0 时出现，且由位置
#      配对点到真正的那些门**（T1214：写死的「（Gate G）」在判定翻面后会把
#      「没有未通过的门」与「Gate G 未通过」印在同一句里）。
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

# 台账规矩：「没有 command 的 passed 不算证据」。这一条**直接数台账**
# （T1210 要求 4e）：空 evidence（或只有空白字符）的 passed 必须为 0。
# 它不是报告的断言，是台账的断言——报告怎么写都绕不过它。
passed_without_evidence="$(jq '
  [ .tests[] | select(.status=="passed")
    | select((((.evidence // "") | gsub("[[:space:]]"; "")) | length) == 0) ] | length
' "$ROOT/tasks/tests.json")"

# ---------------------------------------------------------------------------
# 计数块：报告引用的每个数字都由脚本给出，不许手写。
# **两条路径都打印它**：--emit-tables 用它重建报告第 1.3 节，普通运行也把它
# 原样打出来（同一份数字，脚本一边打印、一边校验报告里那 9 行与它逐字相等）——
# 「打印出来的」与「被校验的」是同一份东西，读输出的人不必去读脚本。
# ---------------------------------------------------------------------------
emit_counts_block() {
  echo '<!-- 计数块：报告引用的每个数字都由脚本给出，不许手写 -->'
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
}

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
  emit_counts_block
  exit 0
fi

echo "EXCLUDED=$EXCLUDE_TASK"
echo "V1_REQUIRED_UNMERGED_COUNT=$unmerged_count"
echo "V1_REQUIRED_UNMERGED_LIST:"
echo "$unmerged_json" | jq -r '.[] | "  - \(.id) [\(.status)] (\(.phase)) \(.title)"'

echo "BLOCKING_NOT_RUN_COUNT=$not_run_count"
echo "$not_run_json" | jq -r '.[] | "  - \(.id) (\(.task_id)) \(.name)"'
echo "PASSED_WITHOUT_EVIDENCE_COUNT=$passed_without_evidence"
echo
emit_counts_block

# ---------------------------------------------------------------------------
# 3. 校验报告文件与强制标记
# ---------------------------------------------------------------------------
if [ ! -f "$REPORT" ]; then
  echo "FAIL: report not found: $REPORT" >&2
  exit 1
fi

# require_marker MARKER — 报告里必须**逐字**出现这一串（grep -qF，整份文件里出现即算）。
#
# **这条保证的边界（T1213 定稿时重跑探针实测踩到，留在这里给下一位改写者）**：
# 它问的是「整份报告里有没有这一串」，不是「该写这一串的那一节写的是不是这一串」。
# 于是**报告里任何一处逐字引文都可能满足它所演示的那条断言**——本报告 6.5 表里
# 为探针留证时，曾把脚本打印的 `FAIL: … missing required marker: <那一串>` 整句抄进去，
# 那条 marker 就被这段引文自己找到，构造样本不再变红（引文把断言的械缴了：
# 该格写着新脚本退出码 1，实测却是 0）。要抄失败信息，就把它省到不再是那条 marker 的
# **逐字子串**（本版的做法：末段省成 `…`）；否则请改这一条的修法（例如限定出现位置），
# 不要指望读者「知道那是引文」。同一类陷阱对下面 `collect_all` 的期望串同样成立。
require_marker() {
  local marker="$1"
  if ! grep -qF "$marker" "$REPORT"; then
    echo "FAIL: report missing required marker: $marker" >&2
    fail=1
  fi
}

# collect_all WHAT REGEX EXPECTED — 把报告里**该句式的每一处出现**都收上来，
# 去重后要求集合里恰好只有台账给的那一个元素（T1210 要求 4d）。
#
# 为什么不是 require_marker：那条只要求「正确的句式至少出现一次」——一份在
# 第 1.3 节写对、却在第 0 节写错的报告能骗过它（变异 M8 实测逃逸）。同一个事实
# 在一个文档里说了几遍，就得几遍都说对。集合为空也走这条失败路径：没有出现
# 同样是失败，只是失败的理由不同（不存在 ≠ 说错了，但都不许过）。
#
# **这条保证的边界要写明白（T1210 最终复核 nit，T1213 复核后照原样保留）**：
# 它保证的是「**已经出现**的每一处都说对」，**不是**「该出现的地方都还在」——
# 把某一句从 12 处删到只剩 1 处、而那一处正确，这里仍然通过（只有全部删光才红，
# 走的是上面那条「集合为空」的失败分支）。要后者得另加**位置/条数**断言。
# 本轮**不加**：那是把「值检查」升级成「排版检查」，条数断言在报告每次增删一句
# 时都会红一次，红的原因与它要防的东西无关——正是这个脚本反复被红咬的那一类。
# 所以它是一个**已知的、写在文档里的**边界，不是一处漏改。
collect_all() { # collect_all WHAT REGEX EXPECTED
  local what="$1" regex="$2" expected="$3" found
  found="$(grep -oE "$regex" "$REPORT" | sort -u)"
  if [ "$found" != "$expected" ]; then
    echo "FAIL: $what — the report's occurrences are not exactly the one the ledger gives" >&2
    echo "--- report says ($(echo "$found" | grep -c .) distinct) ---" >&2
    echo "$found" >&2
    echo "--- ledger says ---" >&2
    echo "$expected" >&2
    fail=1
  fi
}

# 10 个 Gate 标题。这个数组是**一处定义、三处用**：
#   1. 下面这个 require_marker 循环（每节标题都在）；
#   2. 第 5.3 节那两条「几通过 / 几未通过」的**位置配对**——第 N 条处置行属于第 N 个标题
#      （报告里各节处置行的出现顺序与这个数组一致，T1214 在实跑里核对过，见报告第 6 节）；
#   3. 未通过时**点名**的那串门名。
# 顺序与报告一致是这条检查的前提：谁排前面，谁就领走第一条处置行。
GATE_TITLES=("Gate A" "Gate B" "Gate C" "Gate D" "Gate E" "Gate F" "Gate G" "Gate H" "Gate I" "Development System Gate")
for gate in "${GATE_TITLES[@]}"; do
  require_marker "### $gate"
done

# 剩余风险编号（R12/R13/R14 也钉上：报告今天有 14 条，缺任何一条都是缺口不点名）
for r in R1 R2 R3 R4 R5 R6 R7 R8 R9 R10 R11 R12 R13 R14; do
  require_marker "### $r."
done

# 关键事实标记（对应四条已知缺口 + T1206/T1209 缺席清单的处置）
require_marker "产品的 blob 写入路径缺失"
require_marker "reopen 在生产里没有入口"
require_marker "部署的 search 没有接任何 planner / embedder / answer provider"
require_marker '`rddev worker collect` 的证据没说清它评了什么'

# Gate G 按**范围豁免**改判（T1214，owner 五条 L3 裁定）：豁免必须**引规格**，不是引自己。
# 这四条钉的是豁免的书面依据（`docs/02` §4 的三条范围裁定由上面那两句引文覆盖，
# `docs/56` 与 `docs/47` 与 `specs/mcp/tools.json` 的 `scope` 标记各钉一处），
# 外加那一行的判定格本身——把 G1 改回「未通过」或改成一个含糊的词，这里就红。
require_marker "| G1 | MCP semantic tools 完整且权限正确 | **范围豁免**"
require_marker "docs/56_REQUIREMENTS_TRACEABILITY.md"
require_marker "docs/47_MCP_TOOL_CATALOG.md"
require_marker '"scope": "post-v1"'

# blob 那条风险按 `L3-②` 重新定性（T1214）：**事实不变，定性变了**。
# 「范围外，不是欠账」是这一条的新定性，V1.x 的前向说明必须一起在（它同时是
# 将来立账时的前提，㊻ §6 点名了这一点）。
require_marker "范围外，不是欠账"
require_marker "未被使用的上传文件多久清掉"

# T1209 落地后的缺席清单（T1210 重写的那一句）：SAST 与 SBOM 已是总门里的
# 实检行、容器扫描是**有守卫的缺席**并有一份**书面风险接受**。三块都要在：
# 句子（两条实检行 + 一条有守卫的缺席）、ADR 文件、那条守卫行的名字与证据行。
require_marker "SAST 与 SBOM 不再是缺席"
require_marker "容器扫描是唯一剩下的缺席，而且是一条有守卫的缺席"
require_marker "docs/adr/ADR-028-v1-ships-no-container-image.md"
# require_marker 只断言报告**提到**这个路径，它一个字也证明不了那份书面接受还在树里
# （T1210 最终复核 nit）。树层的那道保险在总门 `absence-manifest` 的第三条 witness
# （`grep -c 'no-dockerfile' docs/adr/ADR-028-*.md` 非零），而**那份门不在本脚本内**；
# 这一条是快照层的第二道：ADR 被改名或删掉时，这份校验器自己也会红，而不是继续
# 引用一份不存在的接受。它**只加不减**：上面那条 require_marker 原样保留。
if [ ! -f "$ROOT/docs/adr/ADR-028-v1-ships-no-container-image.md" ]; then
  echo "FAIL: the written risk acceptance the report cites is not in the tree: docs/adr/ADR-028-v1-ships-no-container-image.md" >&2
  fail=1
fi
require_marker "MIN_CHECKS=17"
require_marker "master-security-gate: PASS — 17 check(s) ran and each printed its own evidence"
require_marker "ok   item: container-scan — guarded absence: the 'container-scan' row of this gate is registered"

# Gate H 的 H1 行：判定必须**带限定**（T1210 要求 4b）。这是那一行的判定格，
# 逐字钉住——把这一格改回无边界的「通过」，或改成别的值，这里就红。
require_marker '| H1 | Critical/High security issues = 0 | **通过（限定：扫描面之外仍有一条有守卫的缺席）** |'

# 「四层 Gate 通过」必须有它自己的块（T1210 要求 4c）：逐层给权威与「绿记在哪」
require_marker "### 5.2 「四层 Gate 通过」：逐层的权威与「绿记在哪」"
require_marker "specs/orchestrator/gates.json"
require_marker ".rddev/runtime/gates/"

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

# 报告的基准 commit 必须就是当前 HEAD（数字与 SHA 都不许抄旧树）。
# 与上面三条同一个修法（T1210 要求 4d）：收**全部**基准行、要求集合唯一，
# 不是只看第一处。一份在头部写对基准、却在正文引用里留一条旧 SHA 的报告，
# 必须在这里红——「说对一次」不算数。
actual_head="$(git -C "$ROOT" rev-parse HEAD)"
baseline_lines="$(grep -oE '^> 生成基准：`[0-9a-f]{40}`' "$REPORT" | sort -u)"
baseline_expected="> 生成基准：\`${actual_head}\`"
# 出路 (a) 要指回【报告自己】钉的那个 SHA，而不是当前树的 HEAD：打印 HEAD 会把
# 「去哪棵树核对」说反（出口说的是「在报告自己的基准上跑」）。这里的 head -n1 取的是
# 唯一值——上面已经用 sort -u 收成集合，多于一条时走的是下面那条失败分支，不会走到这里。
report_head="$(printf '%s\n' "$baseline_lines" | grep -oE '[0-9a-f]{40}' | head -n1)"
if [ -z "$baseline_lines" ]; then
  echo "FAIL: report does not state its baseline commit in the expected format" >&2
  fail=1
elif [ "$baseline_lines" != "$baseline_expected" ]; then
  # 基准不符不是「报告写错了」，是「证书过期了」——但这仍然是一次失败。
  cat >&2 <<EOF
FAIL: 报告的基准不是这棵树。

  本审计是【基准快照】（snapshot），不是常驻检查：报告是对某一棵树的证书，
  它钉住的每个数字（v1_required 清单、tests.json 分布、blocking not_run 名单）
  都是在下面那个 SHA 上取的一次快照。HEAD 前进一次（合并、落账、任何一次提交），
  报告就与当前树不同步——这不是报告错了，是它记录的那棵树不是这一棵。

  报告钉在的基准：$baseline_lines
  当前工作树 HEAD：$actual_head

  两条出路：
    (a) 在报告自己的基准上跑这条命令：git checkout ${report_head:-（报告里没有可解析的基准行）}（或那个基准的 worktree），
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
# 这里的两个 head -n1 是**位置**断言（哪一行在前），不是值检查：值检查一律走上面的
# collect_all（收全部出现、要求集合唯一）。位置问的是「第一条在哪一行」，取第一条就是它要问的东西。
first_section_line="$(grep -nE '^## 1\.' "$REPORT" | head -n1 | cut -d: -f1)"
head_line="$(grep -nE '^> 生成基准：`[0-9a-f]{40}`' "$REPORT" | head -n1 | cut -d: -f1)"
if [ -z "$head_line" ] || [ -z "$first_section_line" ] || [ "$head_line" -ge "$first_section_line" ]; then
  echo "FAIL: the report's baseline line must be in the header block, before the first section (line $head_line vs $first_section_line)" >&2
  fail=1
fi

# 校验报告中的计数与脚本计算一致（防止报告悄悄改数字）。
# 两处都收**全部**出现、要求集合唯一（T1210 要求 4d）：与下面分布那一条同一个修法。
counts_target_sentence="**计数：${unmerged_count} / 0"
collect_all "the unmerged-count sentence" '^\*\*计数：[0-9]+ / [0-9]+' "$counts_target_sentence"

not_run_target_sentence="当前结果：**${not_run_count} 条 blocking 测试为"
collect_all "the not_run-count sentence" '^当前结果：\*\*[0-9]+ 条 blocking 测试为' "$not_run_target_sentence"

# 校验报告声明的 tests.json 整体分布与台账一致
distribution_marker="**${total_tests} 条 = ${passed_tests} \`passed\` + ${not_run_count} \`not_run\`**"
collect_all "the ledger-distribution sentence" '\*\*[0-9]+ 条 = [0-9]+ `passed` \+ [0-9]+ `not_run`\*\*' "$distribution_marker"

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

# 分布的第三道断言（T1210 要求 4e）：**空 evidence 的 passed 必须为 0**。
# 台账规矩是「没有真实命令的 passed 不算证据」，而上面两道都只看 status：
# 一条 `{"status":"passed","evidence":""}` 的账目能骗过它们，骗不过这一条。
# 它同样直接数台账，不经过报告。
if [ "$passed_without_evidence" != "0" ]; then
  echo "FAIL: ledger holds $passed_without_evidence passed entr(ies) with empty evidence; a passed row without a command is not evidence" >&2
  fail=1
fi

# §0 的「N 条 L3 已全部获得裁定」不许手写：脚本自己从三个东西里数。
#   左半边 = 报告第 0 节点名的 L3 编号集合（报告必须逐条点名，不能只报个数）；
#   右半边 = 台账（tasks/decisions.md）里出现过的 L3 编号集合。
# 两个集合必须相等，且报告里那句计数的措辞必须等于**数出来的**那个数。
# 这样「漏登记一条 L3」与「条数写错」两种病各有一半会红。
#
# T1214：owner 在 2026-09-23 把五条全部裁定，所以 §0 那句「几条**未获裁定**」要求一句
# 已经不成立的话。改成数**裁定行**，而不是把句子换个说法：
#   数 §0 里以 `**裁定**：` 起头的行数（= 逐条点名过的裁定条数），
#   要求它**等于**同一节里点名的 L3 标签数——「点名了却没说裁定结果」「说了裁定却漏点名」
#   两个方向都会红。措辞由上面那个 L3_WORDS 拼出来，句子里那两个数一个都不许手写。
l3_in_report="$(awk '/^## 0\./,/^## 1\./' "$REPORT" | grep -oE 'L3-[⓪①②③④⑤⑥⑦⑧⑨]' | sort -u)"
l3_in_ledger="$(grep -oE 'L3-[⓪①②③④⑤⑥⑦⑧⑨]' "$ROOT/tasks/decisions.md" | sort -u)"
if [ "$l3_in_report" != "$l3_in_ledger" ]; then
  echo "FAIL: the L3s the report's section 0 names are not the L3s the decisions record holds" >&2
  echo "--- only in report ---" >&2
  comm -23 <(echo "$l3_in_report") <(echo "$l3_in_ledger") >&2
  echo "--- only in the decisions record ---" >&2
  comm -13 <(echo "$l3_in_report") <(echo "$l3_in_ledger") >&2
  fail=1
fi
l3_count="$(echo "$l3_in_report" | grep -c .)"
L3_WORDS=(零 一 二 三 四 五 六 七 八 九)

# 裁定行的条数：§0 里以 `**裁定**：` 起头的行（每一条 L3 一行，句式见报告 §0）。
# 不写成 `grep -c .` 那种「数非空行」——这里数的正是那个句式本身。
l3_rulings="$(awk '/^## 0\./,/^## 1\./' "$REPORT" | grep -cE '^\*\*裁定\*\*：' || true)"
if [ "$l3_rulings" != "$l3_count" ]; then
  echo "FAIL: the report's section 0 names $l3_count L3 label(s) but carries $l3_rulings ruling line(s) (a line is one that starts with **裁定**：) — every named L3 needs its own ruling line" >&2
  fail=1
fi
require_marker "**${L3_WORDS[$l3_count]}条 L3 已全部获得裁定**"

# 门层的「几通过 / 几未通过」也不许手写，必须与各节自己的判定一致
# （T1210 要求 4b：判定与统计必须一致）。数法是数报告自己的**处置行**：
# docs/31 共 10 条 Gate（含 Development System Gate），每节一行 `**处置**：…`；
# 其中以 `**未通过` 起头的那些就是未通过的门数。第 5.3 节的统计句必须逐字
# 等于**数出来的**那个数——把某节判定改成「未通过」而不同步统计，或者只改统计，
# 两种方向都会在这里红（这与上面 L3 那一条是同一个修法：先数再比，不许手写）。
# 「未通过」怎么数（T1210 最终复核 nit，T1213 修）：**先抹掉排版，再数判定**。
# 原来只认紧跟冒号的 `**未通过`，于是 `**处置**：未通过——…`（少了那对星号）
# 这一种写法数不到，而报告正文里已经写着「未通过」——判定与统计的一致性检查
# 被**排版**绕过（构造样本与两次实跑见报告「修订记录」；这不是措辞洁癖：
# 同一句话换个加粗方式就能让统计漏掉一个未通过的门，那个统计正是第 5.3 节
# 与下面那条 require_marker 的依据）。
# 数的是「这一节的判定是不是未通过」，与它用了哪种加粗无关。
dispositions="$(grep -E '^\*\*处置\*\*：' "$REPORT" | sed -E 's/^\*\*处置\*\*：//; s/^[*_[:space:]]+//')"
gates_disposed="$(printf '%s\n' "$dispositions" | grep -c .)"
gates_failed="$(printf '%s\n' "$dispositions" | grep -c '^未通过')"
GATES_TOTAL=10
if [ "$gates_disposed" != "$GATES_TOTAL" ]; then
  echo "FAIL: report has $gates_disposed gate disposition line(s), want $GATES_TOTAL (one per Gate, none missing)" >&2
  fail=1
fi
gates_passed=$((gates_disposed - gates_failed))

# **只有真的有未通过的门时才点名，点的是数出来的那些门**（T1214 修）。
# 原来这里把门名写死成「（Gate G）」，理由是当时确实只有 Gate G 未通过。T1214 把 Gate G
# 按范围豁免改判之后，未通过数变成 0，而那句写死的措辞会把「没有未通过的门」和
# 「Gate G 未通过」印在同一句里——**判定变了而措辞的硬编码没跟上**，正是这份证书反复
# 被咬的那一类病。改法是让名字跟着**数出来的判定**走：
#   - 数法一个字没动（同上：先抹掉排版再数，判定与统计必须一致）；
#   - 门名由**位置配对**得出——报告里第 N 条处置行属于 GATE_TITLES 的第 N 个标题；
#   - 未通过数为 0 时，那句里**不带括号、不点任何门名**。
# 两个方向都被钉住：某节判「未通过」而统计句写 0 → 红；统计句写 1 却没点出是哪一门 → 红。
# 不通过字面量换绿（这里没有任何字面门名可写）。
failed_gates=""
gate_index=0
while IFS= read -r disposition; do
  [ -n "$disposition" ] || continue
  if [ "$gate_index" -lt "$GATES_TOTAL" ]; then
    case "$disposition" in
      未通过*) failed_gates="${failed_gates}${failed_gates:+, }${GATE_TITLES[$gate_index]}" ;;
    esac
  fi
  gate_index=$((gate_index + 1))
done <<< "$dispositions"

if [ "$gates_failed" -eq 0 ]; then
  stats_marker="Gate 层面：**${gates_passed} 条「通过」、0 条「未通过」**"
else
  stats_marker="Gate 层面：**${gates_passed} 条「通过」、${gates_failed} 条「未通过」（${failed_gates}）**"
fi
require_marker "$stats_marker"

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

echo "AUDIT OK: report markers present, counts match ($unmerged_count unmerged, $not_run_count not_run, $failed_tests failed, $skipped_tests skipped, $passed_without_evidence passed-without-evidence)."
exit 0
