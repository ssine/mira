export async function sidebarAction(page, id) {
  await page.locator("#agentView:not(.hidden)").waitFor();
  if (await page.locator("#agentThreadDrawer").getAttribute("aria-hidden") === "true") await page.locator("#agentThreadDrawerToggle").click();
  if (!await page.locator("#agentNavMenu").isVisible()) await page.locator("#agentNavMenuToggle").click();
  await page.locator(`#${id}`).click();
}

export async function accountDetails(page) {
  await page.locator("#agentView:not(.hidden)").waitFor();
  if (await page.locator("#agentThreadDrawer").getAttribute("aria-hidden") === "true") await page.locator("#agentThreadDrawerToggle").click();
  if (!await page.locator("#agentAccountDetails").isVisible()) await page.locator("#agentAccountToggle").click();
}

export async function closeSidebar(page, { touch = false } = {}) {
  const overlay = page.locator("#agentThreadDrawerBackdrop.open");
  const overlaid = await overlay.isVisible();
  const button = overlaid ? overlay : page.locator("#agentThreadDrawerToggle");
  const options = overlaid ? { position: { x: (await overlay.boundingBox()).width - 12, y: 60 } } : {};
  if (touch) await button.tap(options);
  else await button.click(options);
}
