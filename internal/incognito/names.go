package incognito

// firstNames + lastNames are the fantasy-leaning pools used to mint dev
// pseudonyms. Hand-curated to feel human-name-shaped but avoid any obvious
// real-world or IP references. Combination space is 60×60 = 3,600 — plenty
// for a 31-dev cohort with room to grow.
//
// The pools are intentionally not sorted in a meaningful way; assignment
// order through them is hash-driven (see Mapping.assignDev). New entries can
// be appended to either pool without breaking persisted assignments, because
// once a real name is assigned, the mapping is durable on disk and the pool
// is only consulted for new real names.
var firstNames = []string{
	"Aldric", "Brenna", "Cassian", "Daria", "Elowen", "Faelan", "Gareth", "Hollis",
	"Idris", "Jora", "Kaelen", "Liriel", "Marcus", "Nyx", "Orin", "Petra",
	"Quill", "Rowan", "Selene", "Torvin", "Una", "Vesper", "Wren", "Xanthe",
	"Yara", "Zephyr", "Aelric", "Briar", "Corin", "Delphine", "Eamon", "Fenris",
	"Greta", "Halia", "Ilora", "Jasper", "Kira", "Lucan", "Maeve", "Niall",
	"Oriana", "Phaedra", "Quentin", "Rhea", "Soren", "Tamsin", "Ulric", "Verity",
	"Willem", "Xara", "Yorick", "Zinnia", "Auralie", "Branwen", "Cael", "Doria",
	"Emrys", "Floriana", "Gideon", "Hesper",
}

var lastNames = []string{
	"Ashworth", "Blackthorne", "Crowley", "Dunmoor", "Everstone", "Fenwick",
	"Greymane", "Hawthorn", "Ironwood", "Jasper", "Korrigan", "Larkspur",
	"Moonshade", "Nightshade", "Oakheart", "Pendragon", "Quillborne", "Ravencrest",
	"Stormwind", "Thornfield", "Underwood", "Vasholm", "Whitestone", "Yarrow",
	"Zephyrine", "Brambleshade", "Coldwater", "Duskwood", "Elderbrook", "Frostwood",
	"Glimmerfell", "Highmoor", "Inkthorn", "Joriswick", "Karthbridge", "Lornsdale",
	"Mistralwen", "Northwind", "Oldgrove", "Pyrewood", "Quailmarsh", "Rosethorn",
	"Silverbranch", "Tidewater", "Umberhollow", "Vellanore", "Wildmere", "Yewmoor",
	"Zephyrgate", "Briarcliff", "Cinderhall", "Daggerwood", "Embergrove", "Faolan",
	"Greystone", "Hollowfen", "Ironvale", "Juniperhill", "Kessermark", "Loomwood",
}

// projectPrefix is what every anonymized epic name starts with — "Project 1",
// "Project 2", etc. Numbered sequentially in assignment order. Different
// vocabulary from devs because epics carry a different conceptual weight in
// the UI (operational identifier, not a personality).
const projectPrefix = "Project "
