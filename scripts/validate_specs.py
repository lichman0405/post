#!/usr/bin/env python3
"""Lightweight validation for the specification bundle itself."""
from pathlib import Path
import json

ROOT = Path(__file__).resolve().parents[1]

def main():
    tasks = json.loads((ROOT/'tasks/tasks.json').read_text(encoding='utf-8'))
    ids = {t['id'] for t in tasks['tasks']}
    assert len(ids) == tasks['task_count']
    for t in tasks['tasks']:
        for d in t['dependencies']:
            assert d in ids, (t['id'], d)
        assert t['requirements'] and t['acceptance_criteria']
    # cycle check
    m={t['id']:t for t in tasks['tasks']}; vis={}
    def dfs(i):
        assert vis.get(i) != 1, f'cycle at {i}'
        if vis.get(i)==2:return
        vis[i]=1
        for d in m[i]['dependencies']: dfs(d)
        vis[i]=2
    for i in ids: dfs(i)
    for p in (ROOT/'specs/schemas').glob('*.json'):
        json.loads(p.read_text(encoding='utf-8'))
    json.loads((ROOT/'specs/mcp/tools.json').read_text(encoding='utf-8'))
    print(f"OK: {len(ids)} tasks, JSON specs valid")

if __name__ == '__main__': main()
