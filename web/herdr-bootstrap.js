const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-403-a8495c246aededd3/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
