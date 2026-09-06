try {
  document.documentElement.dataset.theme = localStorage.getItem("mira.theme") === "dark" ? "dark" : "light";
} catch {
  document.documentElement.dataset.theme = "light";
}
document.querySelector('meta[name="theme-color"]').content = document.documentElement.dataset.theme === "dark" ? "#1f2226" : "#ffffff";
