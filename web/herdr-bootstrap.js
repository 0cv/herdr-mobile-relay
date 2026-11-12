const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-493-a3c52e24b4c4dbcc/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
