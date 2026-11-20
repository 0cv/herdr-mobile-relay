const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-519-c68aca1bccf33583/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
