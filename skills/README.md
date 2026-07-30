# skills

Claude Code skills for driving a fanoutd board from any repo.

| Skill | What it does |
|---|---|
| [`fanout`](fanout/SKILL.md) | Break an idea into parallel subtasks, run them, watch them, collect the output |

Install by symlinking into your skills directory — the skill stays in the repo,
so `git pull` updates it in place:

```bash
# available everywhere
ln -s "$PWD/skills/fanout" ~/.claude/skills/fanout

# or just this project
ln -s "$PWD/skills/fanout" .claude/skills/fanout
```

The skill assumes `fanout` is on `$PATH` and a board is reachable — see the
[configuration section](../README.md#configuration) of the main README.
