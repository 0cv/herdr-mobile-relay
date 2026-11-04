const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.24.0-487-39ab5e75cd98723f/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
