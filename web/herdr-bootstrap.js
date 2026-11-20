const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-515-a17baf0b46d2a7b5/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
