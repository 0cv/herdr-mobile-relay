const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-388-ef4542f819566d18/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
