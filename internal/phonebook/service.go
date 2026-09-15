package phonebook

import (
	"sort"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
)

// ContactWithGroups represents a contact with its associated groups
type ContactWithGroups struct {
	Contact *database.Contact
	Groups  []*database.GroupNumber
}

// Service provides business logic for phonebook operations
type Service struct {
	db *database.DB
}

// NewService creates a new phonebook service
func NewService(db *database.DB) *Service {
	return &Service{db: db}
}

// GetContactWithGroups retrieves a contact with all its group memberships
func (s *Service) GetContactWithGroups(uid string) (*ContactWithGroups, error) {
	contact, err := s.db.GetContact(uid)
	if err != nil {
		return nil, err
	}
	if contact == nil {
		return nil, nil
	}

	groups, err := s.db.GetContactGroups(contact.ID)
	if err != nil {
		return nil, err
	}

	return &ContactWithGroups{
		Contact: contact,
		Groups:  groups,
	}, nil
}

// SearchContactsWithGroups searches contacts and includes their groups
func (s *Service) SearchContactsWithGroups(query string, limit int) ([]*ContactWithGroups, error) {
	contacts, err := s.db.SearchContacts(query, limit)
	if err != nil {
		return nil, err
	}

	results := make([]*ContactWithGroups, 0, len(contacts))
	for _, contact := range contacts {
		groups, err := s.db.GetContactGroups(contact.ID)
		if err != nil {
			return nil, err
		}

		results = append(results, &ContactWithGroups{
			Contact: contact,
			Groups:  groups,
		})
	}

	return results, nil
}

// ListContactsWithGroups lists contacts with pagination and includes their groups
func (s *Service) ListContactsWithGroups(limit, offset int) ([]*ContactWithGroups, error) {
	contacts, err := s.db.ListContacts(limit, offset)
	if err != nil {
		return nil, err
	}

	results := make([]*ContactWithGroups, 0, len(contacts))
	for _, contact := range contacts {
		groups, err := s.db.GetContactGroups(contact.ID)
		if err != nil {
			return nil, err
		}

		results = append(results, &ContactWithGroups{
			Contact: contact,
			Groups:  groups,
		})
	}

	return results, nil
}

// GroupWithMembers represents a group with its members
type GroupWithMembers struct {
	Group   *database.GroupNumber
	Members []*database.Contact
}

// ActiveMembers filtra Members ai soli contatti disabled=false — la rubrica
// pubblica non deve mai mostrare un nominativo il cui account AD è
// disattivato, anche se il centralino (fonte di verità sui membri del
// gruppo) lo considera ancora appartenente al gruppo di chiamata. Le
// schermate admin (gestione gruppi) continuano a usare Members senza
// filtro: lì serve vedere anche i membri disattivi per poterli rimuovere.
func (g *GroupWithMembers) ActiveMembers() []*database.Contact {
	active := make([]*database.Contact, 0, len(g.Members))
	for _, m := range g.Members {
		if !m.Disabled {
			active = append(active, m)
		}
	}
	return active
}

// GetGroupWithMembers retrieves a group with all its members
func (s *Service) GetGroupWithMembers(id int64) (*GroupWithMembers, error) {
	group, err := s.db.GetGroup(id)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, nil
	}

	members, err := s.db.GetGroupMembers(id)
	if err != nil {
		return nil, err
	}

	return &GroupWithMembers{
		Group:   group,
		Members: members,
	}, nil
}

// ListGroupsWithMembers lists all groups with their member counts
func (s *Service) ListGroupsWithMembers() ([]*GroupWithMembers, error) {
	groups, err := s.db.ListGroups()
	if err != nil {
		return nil, err
	}

	results := make([]*GroupWithMembers, 0, len(groups))
	for _, group := range groups {
		members, err := s.db.GetGroupMembers(group.ID)
		if err != nil {
			return nil, err
		}

		results = append(results, &GroupWithMembers{
			Group:   group,
			Members: members,
		})
	}

	return results, nil
}

// DepartmentGroup è un'intestazione reparto con i contatti al suo interno,
// usata per la vista a gruppi collassabili (dept-head + righe dense in
// search_results.html).
type DepartmentGroup struct {
	Name     string
	Contacts []*ContactWithGroups
}

// GroupByDepartment raggruppa i contatti per Department (etichetta così
// com'è, non normalizzata qui — la sentence-case è responsabilità del
// template). I contatti senza reparto vengono raggruppati per Area invece:
// "politica" -> "Amministrazione politica", altrimenti -> "Senza reparto".
// I gruppi sono sempre ordinati alfabeticamente per nome; i contatti
// mantengono l'ordine di arrivo (i chiamanti passano risultati già
// ordinati per display_name dalla query DB).
// GroupCategoryNode è un nodo dell'albero di categorie per la colonna
// "Chiamate di gruppo" (vedi
// docs/superpowers/specs/2026-09-15-group-categories-hierarchy-design.md).
// Category non è mai nil per i nodi dell'albero — i gruppi senza
// categoria sono il secondo valore di ritorno di BuildGroupCategoryTree,
// non un nodo con Category nil.
type GroupCategoryNode struct {
	Category *database.GroupCategory
	Children []*GroupCategoryNode
	Groups   []*GroupWithMembers
}

// BuildGroupCategoryTree raggruppa groups sotto la categoria più
// specifica che copre il loro numero (database.MatchGroupCategory),
// costruendo l'albero a partire dalla catena ParentID di ogni categoria
// coinvolta. Una categoria senza alcun gruppo (diretto o nei discendenti)
// non compare nell'albero — niente sezioni vuote. Figli e gruppi sono
// ordinati per RangeStart/Number crescente ad ogni livello. I gruppi il
// cui numero non cade in nessun range sono ritornati separatamente
// (uncategorized) — il chiamante li mostra nel bucket "Altre chiamate".
func BuildGroupCategoryTree(groups []*GroupWithMembers, categories []*database.GroupCategory) (tree []*GroupCategoryNode, uncategorized []*GroupWithMembers) {
	nodes := make(map[int64]*GroupCategoryNode, len(categories))
	for _, c := range categories {
		nodes[c.ID] = &GroupCategoryNode{Category: c}
	}

	used := make(map[int64]bool, len(categories))
	for _, g := range groups {
		leaf := database.MatchGroupCategory(g.Group.Number, categories)
		if leaf == nil {
			uncategorized = append(uncategorized, g)
			continue
		}
		nodes[leaf.ID].Groups = append(nodes[leaf.ID].Groups, g)
		used[leaf.ID] = true
	}

	// Marca come "usata" ogni categoria che ha un discendente usato,
	// risalendo la catena — altrimenti un genitore con figli popolati ma
	// senza gruppi propri (es. "Uffici" con solo "Settore VI" popolato)
	// verrebbe scartato insieme al figlio.
	for id := range used {
		c := nodes[id].Category
		for c.ParentID != nil {
			used[*c.ParentID] = true
			c = nodes[*c.ParentID].Category
		}
	}

	var roots []*GroupCategoryNode
	for _, c := range categories {
		if !used[c.ID] {
			continue
		}
		node := nodes[c.ID]
		if c.ParentID == nil {
			roots = append(roots, node)
			continue
		}
		parent, ok := nodes[*c.ParentID]
		if !ok {
			roots = append(roots, node) // genitore inesistente/orfano: tratta come radice
			continue
		}
		parent.Children = append(parent.Children, node)
	}

	sortNodes(roots)
	return roots, uncategorized
}

// sortNodes ordina un livello di nodi per RangeStart crescente (nil per
// ultimo) e ricorre sui figli — stesso criterio di ListGroupCategories.
func sortNodes(nodes []*GroupCategoryNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i].Category.RangeStart, nodes[j].Category.RangeStart
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return *a < *b
	})
	for _, n := range nodes {
		sortNodes(n.Children)
		sort.SliceStable(n.Groups, func(i, j int) bool {
			return n.Groups[i].Group.Number < n.Groups[j].Group.Number
		})
	}
}

func GroupByDepartment(contacts []*ContactWithGroups) []*DepartmentGroup {
	index := make(map[string]int)
	groups := make([]*DepartmentGroup, 0)

	for _, c := range contacts {
		name := c.Contact.Department
		if name == "" {
			if c.Contact.Area == "politica" {
				name = "Amministrazione politica"
			} else {
				name = "Senza reparto"
			}
		}
		i, ok := index[name]
		if !ok {
			i = len(groups)
			index[name] = i
			groups = append(groups, &DepartmentGroup{Name: name})
		}
		groups[i].Contacts = append(groups[i].Contacts, c)
	}

	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].Name < groups[j].Name
	})

	return groups
}
