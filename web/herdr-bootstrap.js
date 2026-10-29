const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.5-482-ee542f7e4ecd37bd/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
