"""Validate the handoff documents; this is not a product acceptance gate."""

import json
import re
from pathlib import Path


def main():
    root = Path(__file__).resolve().parents[2]
    folder = root / "tasks" / "v2"
    backlog = json.loads((folder / "backlog.json").read_text(encoding="utf-8"))
    acceptance = json.loads((folder / "web-acceptance.json").read_text(encoding="utf-8"))
    estimate = json.loads((folder / "estimate.json").read_text(encoding="utf-8"))
    tasks = backlog["tasks"]
    by_id = {task["id"]: task for task in tasks}
    assert len(tasks) == len(by_id) == 31, "工作包重复或数量不一致"
    assert set(by_id) == {f"WEB-{i:02d}" for i in range(31)}, "工作包编号缺失"
    journeys = acceptance["journeys"]
    journey_ids = {case["id"] for case in journeys}
    assert len(journeys) == len(journey_ids) == 14, "网页旅程重复或缺失"
    assert journey_ids == {f"U{i:02d}" for i in range(1, 15)}
    visiting, done = set(), set()

    def visit(task_id):
        assert task_id in by_id, f"未知依赖：{task_id}"
        assert task_id not in visiting, f"依赖环：{task_id}"
        if task_id in done:
            return
        visiting.add(task_id)
        for dep in by_id[task_id]["dependencies"]:
            visit(dep)
        visiting.remove(task_id)
        done.add(task_id)

    for task in tasks:
        visit(task["id"])
        assert task["required_for_v2"] is True
        assert task["initial_status"] == "not_started", "不得伪造任务完成"
        assert task["requirements"] and task["acceptance_criteria"]
        assert set(task["journey_ids"]) <= journey_ids
        assert all(task["git_delivery"][key] for key in (
            "issue", "branch", "commit", "push", "pull_request", "ci", "merge"
        )), "缺少完整 GitHub 交付步骤"
    covered = {j for task in tasks for j in task["journey_ids"]}
    assert covered == journey_ids, "网页旅程没有任务覆盖"
    for case in journeys:
        assert case["execution_status"] == "not_run", "不得伪造产品验收"
        numbers = [step["step"] for step in case["steps"]]
        assert numbers == list(range(1, len(numbers) + 1)) and numbers
        assert case["evidence_required"]
    final_ancestors = set()

    def ancestors(task_id):
        for dep in by_id[task_id]["dependencies"]:
            if dep not in final_ancestors:
                final_ancestors.add(dep)
                ancestors(dep)

    ancestors("WEB-25")
    assert final_ancestors == set(by_id) - {"WEB-25"}, "最终 Gate 未覆盖所有必需工作包"
    assert backlog["drive_required"] is False
    assert estimate["work_packages"] == len(tasks)
    for bound in ("low", "high"):
        total = sum(task["effort_hours_estimate"][bound] for task in tasks)
        assert total == estimate["serial_agent_effort_hours"][0 if bound == "low" else 1]
    plan_path = root / "docs" / "70_WEB_OPERABILITY_V2_PLAN.md"
    plan = plan_path.read_text(encoding="utf-8")
    plan_tasks = re.findall(r"^\| (WEB-\d{2}) ", plan, re.MULTILINE)
    plan_journeys = re.findall(r"^### (U\d{2}) ", plan, re.MULTILINE)
    assert len(plan_tasks) == len(tasks) and set(plan_tasks) == set(by_id)
    assert len(plan_journeys) == len(journeys) and set(plan_journeys) == journey_ids
    docs = [plan_path, root / "V2_CLAUDE_CODE_PROMPT.md"] + [
        root / "docs" / name for name in (
            "71_V2_PRODUCT_DISCOVERY.md", "72_V2_CLAUDE_CODE_HANDOFF.md",
            "73_V2_ESTIMATE_AND_EXECUTION.md",
        )
    ] + [folder / "README.md"]
    for doc in docs:
        body = doc.read_text(encoding="utf-8")
        assert not any(line.rstrip() != line for line in body.splitlines()), f"尾随空白：{doc}"
        for target in re.findall(r"\]\(([^)]+)\)", body):
            if target.startswith(("https://", "http://", "#")):
                continue
            assert (doc.parent / target.split("#", 1)[0]).exists(), f"失效链接：{doc}: {target}"
    print(json.dumps({
        "status": "passed", "work_packages": len(tasks), "journeys": len(journeys),
        "steps": sum(len(case["steps"]) for case in journeys),
        "dependency_graph": "acyclic", "final_gate_coverage": "all_required_packages",
        "product_tests_run": False,
    }, ensure_ascii=False))


if __name__ == "__main__":
    main()
