// Same browser-native regression runs in CI and through local camoufoxctl.
const $ = selector => document.querySelector(selector);
const assert = (condition, message) => { if (!condition) throw Error(message); };
const control = value => fetch("/__test/control", { method: "POST", body: JSON.stringify(value) });
const state = async () => (await fetch("/__test/state")).json();
async function until(predicate, message) {
  const deadline = Date.now() + 12000;
  while (!await predicate()) {
    if (Date.now() > deadline) throw Error(message);
    await new Promise(resolve => setTimeout(resolve, 40));
  }
}
const fill = (input, value) => { input.value = value; input.dispatchEvent(new Event("input", { bubbles: true })); };
const visible = node => !!node?.getBoundingClientRect().height && getComputedStyle(node).visibility !== "hidden";
export async function runQuestionPhase(phase) {
  await until(() => $("#agentView:not(.hidden) .trace-card.question"), "Question was not rendered");
  const form = $("form[data-claude-question]"), card = form?.closest(".trace-card");
  if (phase <= 2) {
    assert(visible(form), "Question form is hidden by the empty-body renderer");
    assert(!card.closest(".tool-group"), "Question collapsed into tool details");
    assert(card.textContent.includes("等待你的回答"), "Missing waiting state");
    assert(form.querySelectorAll("fieldset").length === 3, "Missing questions");
    assert(form.querySelectorAll("input:checked").length === 0, "An answer was selected without the user");
    assert(form.textContent.includes("检查页面交互"), "Option description is missing");
    await until(() => $("#conversationActivityText").textContent === "Claude 等待你的回答", "Activity did not show the waiting state");
  }
  if (phase === 1) {
    form.requestSubmit();
    assert((await state()).answers.length === 0, "Empty answers were submitted");
    assert(!form.querySelector("textarea").validity.valid, "Required answer did not validate");
    // Read-only polling must retain both typed input and keyboard focus.
    const input = form.querySelector("textarea");
    input.focus(); fill(input, "草稿");
    await control({ action: "poll" });
    await new Promise(resolve => setTimeout(resolve, 1800));
    assert($("form[data-claude-question]") === form && document.activeElement === input && input.value === "草稿", "Polling replaced the draft or focus");
  } else if (phase === 2) {
    const fields = [...form.querySelectorAll("fieldset")];
    const radios = fields[0].querySelectorAll("input"); radios[0].click(); radios[1].click();
    assert(!radios[0].checked && radios[1].checked, "Single choice did not replace selection");
    const checks = fields[1].querySelectorAll("input"); checks[0].click(); checks[1].click(); checks[0].click();
    assert(!checks[0].checked && checks[1].checked, "Multiple choice cannot be deselected");
    checks[0].click();
    fill(fields[0].querySelector("textarea"), "稍后执行");
    fill(fields[2].querySelector("textarea"), "请保留备份");
    await control({ action: "fail" }); form.requestSubmit();
    await until(() => form.textContent.includes("提交失败"), "Failure not shown inline");
    assert(!form.querySelector("button").disabled && fields[2].querySelector("textarea").value === "请保留备份", "Failure lost answers or prevented retry");
    await control({ action: "hold" }); form.requestSubmit(); form.requestSubmit();
    await until(async () => (await state()).held, "Submission was not held");
    await control({ action: "poll" });
    await new Promise(resolve => setTimeout(resolve, 1800));
    assert((await state()).answers.length === 2, "Duplicate submission was sent");
    assert([...form.elements].every(control => control.disabled), "Poll re-enabled an in-flight form");
    await control({ action: "release" });
    await until(() => !$("form[data-claude-question]"), "Answered form remains actionable");
    const submitted = (await state()).answers;
    assert(JSON.stringify(submitted[0]) === JSON.stringify(submitted[1]), "Retry changed answers");
    assert(submitted[1].answers["这次部署到哪里？"] === "WSL\n稍后执行", "Single choice/custom text mapping is wrong");
    assert(submitted[1].answers["需要执行哪些检查？"] === "单元测试, 浏览器测试", "Multiple choice mapping is wrong");
    assert(submitted[1].answers["还有哪些要求？"] === "请保留备份", "Free text mapping is wrong");
    await until(() => $(".trace-card.question").textContent.includes("已回答"), "Answer summary missing");
    await control({ action: "question" });
    await until(() => $("form[data-claude-question]"), "Second question missing");
    await control({ action: "end" });
    await until(() => !$("form[data-claude-question]"), "Question still actionable after reconciliation without new events");
    assert($("#conversationTrace").textContent.includes("此问题不再等待回答"), "Expired question has no explanation");
  } else if (phase === 3) {
    assert(!form, "Reload resurrected an answered or cancelled question");
    assert($("#conversationTrace").textContent.includes("请保留备份"), "Reload lost the recorded answer");
    const notice = $("#claudePersistence"), checkbox = $("#claudeContinue");
    assert(visible(notice) && visible(checkbox), "Missing incomplete-history explanation");
    assert(notice.textContent.includes("缺失内容不会自动恢复"), "Confirmation does not explain its effect");
    const bounds = notice.getBoundingClientRect(), composer = $("#conversationForm").getBoundingClientRect();
    assert(Math.abs(bounds.left - composer.left) <= 1 && Math.abs(bounds.width - composer.width) <= 1, "History notice is not aligned with the composer");
    assert(!checkbox.checked, "Incomplete history was acknowledged without input");
    await until(() => !$("#conversationInput").disabled, "Composer not ready");
    fill($("#conversationInput"), "继续检查部署结果"); $("#conversationForm").requestSubmit();
    await until(() => $("#conversationNotice").textContent.includes("请先勾选"), "Missing continuation guidance");
    assert((await state()).turns.length === 0, "Unacknowledged history started a turn");
    assert($("#conversationInput").value === "继续检查部署结果", "Confirmation lost the message draft");
    checkbox.click(); $("#conversationForm").requestSubmit();
    await until(async () => (await state()).turns.length === 1, "Acknowledged continuation was not sent");
    assert((await state()).turns[0].continueAcknowledgedHistory === true, "Continuation acknowledgement missing from request");
    await until(() => !checkbox.checked && !visible(checkbox), "Confirmation was not cleared for the next turn");
  }
  assert(document.documentElement.scrollWidth <= innerWidth, "Question causes horizontal overflow");
  return `phase ${phase} passed`;
}
const phase = Number(new URL(import.meta.url).searchParams.get("phase"));
if (phase) runQuestionPhase(phase).then(result => document.documentElement.dataset.questionTest = result)
  .catch(error => { document.documentElement.dataset.questionTest = error.message; console.error(error); });
