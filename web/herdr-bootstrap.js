const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.5-396-4dd12c76cda3174a/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
