const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-401-a562bac854196a7a/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
