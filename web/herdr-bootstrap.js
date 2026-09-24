const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.0-390-7c82458a7171183d/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
