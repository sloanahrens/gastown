package api

// SchemaLevel is the wire schema level this build speaks. It must equal the
// number of migrations in the store (store.SchemaLevel); TestAPISchemaLevelEqualsTheStoresMigrationCount
// in the store package holds the two together. It lives here so a client can send it
// without linking the database driver.
const SchemaLevel = 3
