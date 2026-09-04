try {
  document.documentElement.dataset.theme = localStorage.getItem("mira.theme") === "dark" ? "dark" : "light";
} catch {
  document.documentElement.dataset.theme = "light";
}
