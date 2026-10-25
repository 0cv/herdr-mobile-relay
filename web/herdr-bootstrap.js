const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.2-472-e744e7c59c2e50ee/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
