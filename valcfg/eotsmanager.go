package valcfg

const (
	DefaultEOTSManagerDBBackend = "bbolt"
	DefaultEOTSManagerDBPath    = "bbolt-eots.db"
	DefaultEOTSManagerDBName    = "eots-default"
)

type EOTSManagerConfig struct {
	DBBackend string `long:"dbbackend" description:"Possible database to choose as backend"`
	DBPath    string `long:"dbpath" description:"The path that stores the database file"`
	DBName    string `long:"dbname" description:"The name of the database"`
}

func DefaultEOTSManagerConfig() EOTSManagerConfig {
	return EOTSManagerConfig{
		DBBackend: DefaultEOTSManagerDBBackend,
		DBPath:    DefaultEOTSManagerDBPath,
		DBName:    DefaultEOTSManagerDBName,
	}
}