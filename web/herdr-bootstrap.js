const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.5-486-a7f163ea21911107/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
