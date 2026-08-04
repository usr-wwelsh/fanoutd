// One vocabulary for the review stage, shared by every surface that shows it.
//
// A verdict is a fact about a row, so all of this is derived from a task the
// board already polled rather than fetched again: the column says where the work
// is, the status says whether anything is happening to it, and the verdict says
// what somebody else made of it. The three together are what a card has to say
// in one line.
//
// The only string shared with the server is the trace prefix. internal/agent/
// review.go writes it in front of every step a review pass records, and it is
// the sole mark separating the reviewer's transcript from the author's — the two
// are kept on the same task on purpose, so the verdict is visible where the work
// is.

export const REVIEW_PREFIX = 'review: ';

export function isReviewStep(step) {
  return (step.action ?? '').startsWith(REVIEW_PREFIX);
}

// The action without the prefix. A lane labelled REVIEW does not need every row
// inside it to repeat the word.
export function stepAction(step) {
  const action = step.action ?? '';
  return isReviewStep(step) ? action.slice(REVIEW_PREFIX.length) : action;
}

// The terminal steps a review pass writes. Everything else it records is an
// ordinary read of the work.
const VERDICT_STEPS = {
  passed: { key: 'passed', label: 'Passed', tone: 'done' },
  rejected: { key: 'rejected', label: 'Sent back', tone: 'judge' },
  'no verdict': { key: 'none', label: 'No verdict', tone: 'fault' },
};

export function verdictStep(step) {
  return isReviewStep(step) ? (VERDICT_STEPS[stepAction(step)] ?? null) : null;
}

// Where a task stands with a reviewer, or null for work review has nothing to
// say about — anything that never reached the column, and everything at all when
// the stage is switched off.
//
// Order is worst-first for the same reason the plan's rollup is: a task parked
// in review carrying an error is waiting for a person, whatever else is true of
// it.
export function reviewState(task) {
  const inReview = task.column === 'review';

  if (inReview && task.status === 'error') {
    return task.verdict === 'rejected'
      ? { key: 'stalled', label: 'Needs a person', tone: 'fault',
          hint: 'Sent back as many times as it is allowed to be. Nothing further will run on it.' }
      : { key: 'stalled', label: 'No verdict', tone: 'fault',
          hint: 'The review did not reach pass or reject, so the work was neither accepted nor faulted.' };
  }
  if (inReview && task.status === 'running') {
    return { key: 'judging', label: 'Under review', tone: 'judge',
             hint: 'A second agent is checking this against the criteria. It can read and run the work but not change it.' };
  }
  if (task.verdict === 'rejected') {
    return { key: 'rejected', label: 'Sent back', tone: 'judge',
             hint: 'Review found the criteria unmet. A rework task carries the findings.' };
  }
  if (task.verdict === 'passed') {
    return { key: 'passed', label: 'Review passed', tone: 'done',
             hint: 'A second agent checked this against the criteria and found nothing wrong.' };
  }
  if (inReview) {
    return { key: 'awaiting', label: 'Awaiting verdict', tone: 'judge',
             hint: 'The agent signed this off. Nothing has checked it yet.' };
  }
  return null;
}

// Worst-first across a set, for the one badge a plan or a group card shows. The
// order is the rollup's: something happening beats something wrong, which beats
// something unanswered, which beats an answer.
const RANK = ['stalled', 'judging', 'rejected', 'awaiting', 'passed'];

export function rollupReview(tasks) {
  let best = null;
  for (const task of tasks) {
    const state = reviewState(task);
    if (!state) continue;
    if (!best || RANK.indexOf(state.key) < RANK.indexOf(best.key)) best = state;
  }
  return best;
}

// The review stage as a sequence, from the first verdict on the work to
// whatever the last rework got. It is what the plan view draws underneath the
// waves, so that a rework is part of the picture of the plan it repairs rather
// than an unexplained task somewhere else on the board.
//
// One thing here is reconstructed rather than read. A rework that passes files
// the work it repaired as well as itself, which overwrites the rejection that
// opened it — so a superseded verdict is not on the row any more. The rework is:
// its existence says the round before it was sent back, and its goal is the
// findings that sent it back, verbatim. Nothing is inferred that the tasks do
// not already say.
export function reviewTimeline(rootTasks, reworks, anchorId) {
  const bands = [];
  const stages = [{ tasks: rootTasks, anchorId }, ...reworks.map(t => ({ tasks: [t], task: t, anchorId: t.id }))];

  stages.forEach((stage, i) => {
    if (stage.task) bands.push({ kind: 'rework', key: `r:${stage.task.id}`, task: stage.task });

    const nextRound = reworks[i];
    if (nextRound) {
      bands.push({
        kind: 'verdict',
        key: `v:${stage.anchorId}`,
        state: { key: 'rejected', label: 'Sent back', tone: 'judge' },
        note: nextRound.goal,
        traceId: stage.anchorId,
        round: nextRound.review_round,
      });
      return;
    }
    const state = rollupReview(stage.tasks);
    if (state) {
      bands.push({
        kind: 'verdict',
        key: `v:${stage.anchorId}`,
        state,
        note: noteOf(stage.tasks) || state.hint,
        traceId: stage.anchorId,
        round: 0,
      });
    }
  });
  return bands;
}

// What the reviewer said, from whichever of the tasks it covers carries it. One
// verdict is filed against every task in its scope, so any of them will do and
// the first non-empty one is the least surprising.
function noteOf(tasks) {
  for (const t of tasks) {
    if ((t.verdict_note ?? '').trim()) return t.verdict_note.trim();
  }
  return '';
}

// criteriaLines splits the stored newline-separated criteria into the list both
// the agent and the reviewer were shown.
export function criteriaLines(criteria) {
  return (criteria ?? '').split('\n').map(s => s.trim()).filter(Boolean);
}

// isRework tells a review's repair pass from the other children a task can have.
// Retrying and continuing both write a parent link too, and neither was opened
// by a verdict; only a rework carries a round.
export function isRework(task) {
  return task.review_round > 0 && !!task.parent_id;
}

// The chain of reworks hanging off a task, in the order they were opened. A
// rejection creates one task whose parent is the work it repairs, so a second
// round is a rework of a rework and the chain is walked rather than filtered.
//
// It is bounded by the tasks it is given, and a parent link pointing at itself
// or backwards cannot loop it.
export function reworkChain(rootIds, tasks) {
  const chain = [];
  const seen = new Set(rootIds);
  let frontier = new Set(rootIds);
  while (frontier.size) {
    const next = tasks.filter(t => isRework(t) && frontier.has(t.parent_id) && !seen.has(t.id));
    if (!next.length) break;
    for (const task of next) {
      seen.add(task.id);
      chain.push(task);
    }
    frontier = new Set(next.map(t => t.id));
  }
  return chain;
}
