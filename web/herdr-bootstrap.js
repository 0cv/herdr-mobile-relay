const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.0-379-d16c0c8dee2111cb/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
